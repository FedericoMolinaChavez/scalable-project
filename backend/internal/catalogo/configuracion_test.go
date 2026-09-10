package catalogo_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// La configuración del negocio contra la base real: RF-14, RF-15, RF-17, RF-30,
// RF-31, y la auditoría de RNF-36 que las acompaña a todas.
//
// Lo que estas pruebas comprueban no existe en un doble: que la fila de
// auditoría caiga DENTRO de la misma transacción que el cambio, que un rechazo
// del motor —una zona horaria inventada, una regla que cruza medianoche— aborte
// la operación entera, y que politica_version sea append-only de verdad.
//
// Todo se crea nuevo en cada prueba y nada se borra: el tenant de pruebas
// acumula filas, igual que con las reservas, y por eso cada una usa nombres
// únicos en vez de compartir un catálogo fijo.

func servicio(t *testing.T) (*catalogo.Servicio, *datos.BD, uuid.UUID) {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	return catalogo.Nuevo(bd), bd, uuid.MustParse(pruebas.Tenant)
}

// actor es un administrador con identidad, que es lo que exige
// auditoria_actor_coherente para todo lo que no sea el sistema.
func actor(t *testing.T, bd *datos.BD) auditoria.Actor {
	t.Helper()

	return auditoria.Actor{
		Tipo:        "administrador",
		ID:          cuentaAdministradora(t, bd),
		IP:          "203.0.113.9",
		Dispositivo: "pruebas",
	}
}

func unico(t *testing.T, prefijo string) string {
	t.Helper()
	return prefijo + " " + t.Name() + " " + uuid.NewString()[:8]
}

// ------------------------------------------------------------------ RF-30 --

// El ciclo completo: una sede, un servicio en ella y un recurso que lo presta.
// Los tres pasos son lo que hace falta para que exista algo reservable, y el
// último es el que más se olvida: sin el enlace servicio/recurso, el recurso
// existe y el núcleo lo rechaza.
func TestElCicloDeAltaDejaAlgoReservable(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	sede, err := svc.CrearSede(t.Context(), tenant, quien, api.NuevaSede{
		Nombre:      unico(t, "Sede"),
		ZonaHoraria: "America/Bogota",
	})
	if err != nil {
		t.Fatalf("CrearSede devolvió error: %v", err)
	}
	if sede.Estado != api.EstadoCatalogoActivo {
		t.Fatalf("una sede nueva nace en %q y debería nacer activa", sede.Estado)
	}

	servicioNuevo, err := svc.CrearServicio(t.Context(), tenant, quien, api.NuevoServicio{
		SedeId:      sede.Id,
		Nombre:      unico(t, "Servicio"),
		DuracionMin: 30,
		PrecioMonto: "45000.00",
	})
	if err != nil {
		t.Fatalf("CrearServicio devolvió error: %v", err)
	}
	if servicioNuevo.Precio.Moneda != "COP" {
		t.Fatalf("la moneda salió %q; sale del tenant (RF-38), no del servicio",
			servicioNuevo.Precio.Moneda)
	}

	recurso, err := svc.CrearRecurso(t.Context(), tenant, quien, api.NuevoRecurso{
		SedeId:    sede.Id,
		Nombre:    unico(t, "Recurso"),
		Servicios: &[]uuid.UUID{servicioNuevo.Id},
	})
	if err != nil {
		t.Fatalf("CrearRecurso devolvió error: %v", err)
	}
	if recurso.Servicios == nil || len(*recurso.Servicios) != 1 ||
		(*recurso.Servicios)[0] != servicioNuevo.Id {
		t.Fatalf("el recurso no quedó enlazado al servicio: %v", recurso.Servicios)
	}
}

// Desactivar una sede la saca del catálogo público sin borrarla: el
// administrador la sigue viendo, porque necesita poder volver a encenderla.
func TestUnaSedeInactivaSaleDelCatalogoPublicoYNoDelDelAdministrador(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	sede, err := svc.CrearSede(t.Context(), tenant, quien, api.NuevaSede{
		Nombre:      unico(t, "Sede"),
		ZonaHoraria: "America/Bogota",
	})
	if err != nil {
		t.Fatalf("CrearSede devolvió error: %v", err)
	}

	inactivo := api.EstadoCatalogoInactivo
	if _, err := svc.ActualizarSede(t.Context(), tenant, quien, sede.Id,
		api.ActualizacionSede{Estado: &inactivo}); err != nil {
		t.Fatalf("ActualizarSede devolvió error: %v", err)
	}

	publicas, err := svc.Sedes(t.Context(), tenant)
	if err != nil {
		t.Fatalf("Sedes devolvió error: %v", err)
	}
	if contieneSede(publicas, sede.Id) {
		t.Error("una sede inactiva sigue apareciendo en el catálogo público")
	}

	todas, err := svc.SedesTodas(t.Context(), tenant)
	if err != nil {
		t.Fatalf("SedesTodas devolvió error: %v", err)
	}
	if !contieneSede(todas, sede.Id) {
		t.Error("el administrador no ve la sede que acaba de desactivar")
	}
}

func contieneSede(sedes []api.Sede, id uuid.UUID) bool {
	for _, sede := range sedes {
		if sede.Id == id {
			return true
		}
	}
	return false
}

// ------------------------------------------------------------------ RF-36 --

// La auditoría cae en la MISMA transacción que el cambio (RNF-36). Es lo que
// hace que no exista una acción sin traza: no hay ventana entre una cosa y la
// otra en la que un fallo pueda dejar solo la primera.
func TestCadaCambioDejaSuEventoDeAuditoria(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)
	consulta := auditoria.NuevaConsulta(bd)

	antes := time.Now()

	sede, err := svc.CrearSede(t.Context(), tenant, quien, api.NuevaSede{
		Nombre:      unico(t, "Sede"),
		ZonaHoraria: "America/Bogota",
	})
	if err != nil {
		t.Fatalf("CrearSede devolvió error: %v", err)
	}

	lista, err := consulta.Listar(t.Context(), tenant, auditoria.Filtro{
		Desde:       &antes,
		RecursoTipo: auditoria.RecursoSede,
		Accion:      "crear_sede",
	})
	if err != nil {
		t.Fatalf("Listar la auditoría devolvió error: %v", err)
	}

	var evento *api.EventoAuditoria
	for i := range lista.Datos {
		if lista.Datos[i].RecursoId != nil && *lista.Datos[i].RecursoId == sede.Id {
			evento = &lista.Datos[i]
			break
		}
	}
	if evento == nil {
		t.Fatal("crear una sede no dejó su evento de auditoría")
	}

	if evento.Resultado != api.Exito {
		t.Errorf("el evento dice %q y la operación salió bien", evento.Resultado)
	}
	if evento.ActorTipo != api.EventoAuditoriaActorTipoAdministrador {
		t.Errorf("el actor quedó como %q y era un administrador", evento.ActorTipo)
	}
	if evento.ActorId == nil || evento.ActorId.String() != quien.ID {
		t.Errorf("el evento no dice quién lo hizo: %v", evento.ActorId)
	}
	if evento.Ip == nil || *evento.Ip != "203.0.113.9" {
		t.Errorf("el evento no registró de dónde vino: %v", evento.Ip)
	}
}

// Un RECHAZO también se audita, y no es completitud decorativa: la traza que
// solo registra lo que salió bien no sirve para investigar nada, porque un
// ataque es exactamente una sucesión de intentos que fallaron.
func TestUnRechazoTambienDejaTraza(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)
	consulta := auditoria.NuevaConsulta(bd)

	antes := time.Now()

	// El motor valida la zona horaria contra su propia base de zonas.
	_, err := svc.CrearSede(t.Context(), tenant, quien, api.NuevaSede{
		Nombre:      unico(t, "Sede fantasma"),
		ZonaHoraria: "America/Bogata",
	})
	if err == nil {
		t.Fatal("se creó una sede con una zona horaria que no existe")
	}
	if !errors.Is(err, datos.ErrRestriccion) {
		t.Fatalf("el error no vino del motor: %v", err)
	}

	lista, err := consulta.Listar(t.Context(), tenant, auditoria.Filtro{
		Desde:  &antes,
		Accion: "crear_sede",
	})
	if err != nil {
		t.Fatalf("Listar la auditoría devolvió error: %v", err)
	}

	var rechazos int
	for _, evento := range lista.Datos {
		if evento.Resultado == api.Rechazo {
			rechazos++
		}
	}
	if rechazos == 0 {
		t.Fatal("el intento rechazado no quedó registrado")
	}
}

// ------------------------------------------------------------------ RF-14 --

func TestLasReglasYLasExcepcionesSePublicanYSeRetiran(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	sede, servicioNuevo, recurso := montarCatalogo(t, svc, tenant, quien)
	_ = servicioNuevo

	regla, err := svc.CrearRegla(t.Context(), tenant, quien, api.NuevaRegla{
		RecursoId:  recurso.Id,
		DiaSemana:  2,
		HoraInicio: "09:00",
		HoraFin:    "17:00",
	})
	if err != nil {
		t.Fatalf("CrearRegla devolvió error: %v", err)
	}
	if regla.HoraInicio != "09:00" || regla.HoraFin != "17:00" {
		t.Fatalf("las horas volvieron como %q–%q", regla.HoraInicio, regla.HoraFin)
	}

	// Una regla no cruza medianoche: el motor exige hora_fin > hora_inicio, y
	// un turno de noche son DOS reglas.
	if _, err := svc.CrearRegla(t.Context(), tenant, quien, api.NuevaRegla{
		RecursoId:  recurso.Id,
		DiaSemana:  2,
		HoraInicio: "22:00",
		HoraFin:    "02:00",
	}); !errors.Is(err, datos.ErrRestriccion) {
		t.Fatalf("se aceptó una regla que cruza medianoche: %v", err)
	}

	reglas, err := svc.Reglas(t.Context(), tenant, &recurso.Id)
	if err != nil {
		t.Fatalf("Reglas devolvió error: %v", err)
	}
	if len(reglas) != 1 {
		t.Fatalf("el recurso tiene %d reglas y se creó una", len(reglas))
	}

	// Una excepción tapa una sede o un recurso, y exactamente uno.
	if _, err := svc.CrearExcepcion(t.Context(), tenant, quien, api.NuevaExcepcion{
		Periodo: api.Periodo{Inicio: pruebas.Miercoles(9), Fin: pruebas.Miercoles(10)},
		Tipo:    api.Feriado,
	}); !errors.Is(err, catalogo.ErrExcepcionSinObjetivo) {
		t.Fatalf("se aceptó una excepción que no tapa nada: %v", err)
	}

	excepcion, err := svc.CrearExcepcion(t.Context(), tenant, quien, api.NuevaExcepcion{
		SedeId:  &sede.Id,
		Periodo: api.Periodo{Inicio: pruebas.Miercoles(9), Fin: pruebas.Miercoles(10)},
		Tipo:    api.Feriado,
	})
	if err != nil {
		t.Fatalf("CrearExcepcion devolvió error: %v", err)
	}

	if err := svc.EliminarRegla(t.Context(), tenant, quien, regla.Id); err != nil {
		t.Fatalf("EliminarRegla devolvió error: %v", err)
	}
	if err := svc.EliminarExcepcion(t.Context(), tenant, quien, excepcion.Id); err != nil {
		t.Fatalf("EliminarExcepcion devolvió error: %v", err)
	}

	// Retirar dos veces no es un error distinto de retirar algo que no existe:
	// bajo RLS son indistinguibles y así debe seguir siendo.
	if err := svc.EliminarRegla(
		t.Context(), tenant, quien, regla.Id,
	); !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("retirar una regla ya retirada dio %v", err)
	}
}

// ------------------------------------------------------------------ RF-15 --

// El número de versión lo asigna el servidor, no el cliente. Dejarlo fuera
// permitiría publicar una versión 3 después de una 7 y romper el orden del que
// depende "cuál está vigente".
func TestPublicarUnaPoliticaIncrementaSuVersion(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	_, servicioNuevo, _ := montarCatalogo(t, svc, tenant, quien)

	primera, err := svc.PublicarPolitica(t.Context(), tenant, quien, api.NuevaPolitica{
		ServicioId:             &servicioNuevo.Id,
		RangoCancelacionHoras:  24,
		RangoModificacionHoras: 12,
	})
	if err != nil {
		t.Fatalf("PublicarPolitica devolvió error: %v", err)
	}
	if primera.Version != 1 {
		t.Fatalf("la primera versión de un servicio nuevo es la %d", primera.Version)
	}

	segunda, err := svc.PublicarPolitica(t.Context(), tenant, quien, api.NuevaPolitica{
		ServicioId:             &servicioNuevo.Id,
		RangoCancelacionHoras:  48,
		RangoModificacionHoras: 24,
	})
	if err != nil {
		t.Fatalf("segunda PublicarPolitica devolvió error: %v", err)
	}
	if segunda.Version != 2 {
		t.Fatalf("la segunda versión salió como la %d", segunda.Version)
	}

	// Y la primera sigue ahí: publicar no reemplaza, añade. Es lo que impide
	// que un cambio de política alcance a quien ya reservó.
	politicas, err := svc.Politicas(t.Context(), tenant)
	if err != nil {
		t.Fatalf("Politicas devolvió error: %v", err)
	}

	var encontradas int
	for _, politica := range politicas {
		if politica.Id == primera.Id || politica.Id == segunda.Id {
			encontradas++
		}
	}
	if encontradas != 2 {
		t.Fatalf("se encontraron %d de las 2 versiones publicadas", encontradas)
	}
}

// ------------------------------------------------------------------ RF-17 --

func TestElVoucherSeCreaYSeEliminaPeroNoSeEdita(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	codigo := "PRB" + strings.ToUpper(uuid.NewString()[:8])
	limite := 10

	voucher, err := svc.CrearVoucher(t.Context(), tenant, quien, api.NuevoVoucher{
		Codigo:     codigo,
		Porcentaje: "15.00",
		LimiteUsos: &limite,
	})
	if err != nil {
		t.Fatalf("CrearVoucher devolvió error: %v", err)
	}
	if voucher.Estado != api.VoucherEstadoActivo {
		t.Fatalf("el voucher nace en %q", voucher.Estado)
	}

	// Sin límite de usos ni caducidad no se crea: un voucher del 50% sin
	// ninguno de los dos es una fuga de ingresos abierta para siempre.
	if _, err := svc.CrearVoucher(t.Context(), tenant, quien, api.NuevoVoucher{
		Codigo:     codigo + "X",
		Porcentaje: "50.00",
	}); !errors.Is(err, datos.ErrRestriccion) {
		t.Fatalf("se creó un voucher sin final: %v", err)
	}

	// El código no se repite dentro del tenant, sin distinguir mayúsculas.
	if _, err := svc.CrearVoucher(t.Context(), tenant, quien, api.NuevoVoucher{
		Codigo:     strings.ToLower(codigo),
		Porcentaje: "10.00",
		LimiteUsos: &limite,
	}); !errors.Is(err, datos.ErrDuplicado) {
		t.Fatalf("se repitió un código de voucher: %v", err)
	}

	if err := svc.EliminarVoucher(t.Context(), tenant, quien, voucher.Id); err != nil {
		t.Fatalf("EliminarVoucher devolvió error: %v", err)
	}

	// Borrado LÓGICO: la fila permanece porque uso_voucher la referencia, y su
	// código sigue ocupado para que dos promociones distintas no compartan uno.
	vouchers, err := svc.Vouchers(t.Context(), tenant)
	if err != nil {
		t.Fatalf("Vouchers devolvió error: %v", err)
	}

	var visto bool
	for _, v := range vouchers {
		if v.Id == voucher.Id {
			visto = true
			if v.Estado != api.VoucherEstadoEliminado {
				t.Fatalf("el voucher eliminado sigue en %q", v.Estado)
			}
		}
	}
	if !visto {
		t.Fatal("el voucher eliminado desapareció de la tabla")
	}
}

// ------------------------------------------------------------------ RF-31 --

func TestLasTarifasSeListanPorPrioridad(t *testing.T) {
	svc, bd, tenant := servicio(t)
	quien := actor(t, bd)

	_, servicioNuevo, _ := montarCatalogo(t, svc, tenant, quien)

	baja, alta := 1, 10
	if _, err := svc.CrearTarifa(t.Context(), tenant, quien, api.NuevaTarifa{
		ServicioId: servicioNuevo.Id,
		Tipo:       api.DiaSemana,
		Condicion:  map[string]any{"dia": 6},
		Monto:      "60000.00",
		Prioridad:  &baja,
	}); err != nil {
		t.Fatalf("CrearTarifa devolvió error: %v", err)
	}

	dominante, err := svc.CrearTarifa(t.Context(), tenant, quien, api.NuevaTarifa{
		ServicioId: servicioNuevo.Id,
		Tipo:       api.FranjaHoraria,
		Condicion:  map[string]any{"desde": "18:00", "hasta": "22:00"},
		Monto:      "90000.00",
		Prioridad:  &alta,
	})
	if err != nil {
		t.Fatalf("segunda CrearTarifa devolvió error: %v", err)
	}

	tarifas, err := svc.Tarifas(t.Context(), tenant, &servicioNuevo.Id)
	if err != nil {
		t.Fatalf("Tarifas devolvió error: %v", err)
	}
	if len(tarifas) != 2 {
		t.Fatalf("se listaron %d tarifas y se crearon 2", len(tarifas))
	}

	// La de mayor prioridad primero: con dos que aplican a la vez, el orden es
	// la respuesta a cuál gana, y sin él dependería del planificador.
	if tarifas[0].Id != dominante.Id {
		t.Fatalf("la primera tarifa es %s y debería ser la de prioridad %d", tarifas[0].Id, alta)
	}
	if tarifas[0].Condicion["desde"] != "18:00" {
		t.Fatalf("la condición no volvió como se guardó: %v", tarifas[0].Condicion)
	}

	if err := svc.EliminarTarifa(t.Context(), tenant, quien, dominante.Id); err != nil {
		t.Fatalf("EliminarTarifa devolvió error: %v", err)
	}
}

// ------------------------------------------------------------- auxiliares --

// montarCatalogo deja una sede, un servicio y un recurso enlazados, que es lo
// mínimo sobre lo que se puede configurar cualquier otra cosa.
func montarCatalogo(
	t *testing.T, svc *catalogo.Servicio, tenant uuid.UUID, quien auditoria.Actor,
) (api.Sede, api.Servicio, api.Recurso) {
	t.Helper()

	sede, err := svc.CrearSede(t.Context(), tenant, quien, api.NuevaSede{
		Nombre:      unico(t, "Sede"),
		ZonaHoraria: "America/Bogota",
	})
	if err != nil {
		t.Fatalf("no se pudo crear la sede de la prueba: %v", err)
	}

	servicioNuevo, err := svc.CrearServicio(t.Context(), tenant, quien, api.NuevoServicio{
		SedeId:      sede.Id,
		Nombre:      unico(t, "Servicio"),
		DuracionMin: 60,
		PrecioMonto: "80000.00",
	})
	if err != nil {
		t.Fatalf("no se pudo crear el servicio de la prueba: %v", err)
	}

	recurso, err := svc.CrearRecurso(t.Context(), tenant, quien, api.NuevoRecurso{
		SedeId:    sede.Id,
		Nombre:    unico(t, "Recurso"),
		Servicios: &[]uuid.UUID{servicioNuevo.Id},
	})
	if err != nil {
		t.Fatalf("no se pudo crear el recurso de la prueba: %v", err)
	}

	return sede, servicioNuevo, recurso
}

// cuentaAdministradora crea la cuenta que firma las acciones de la prueba.
//
// Hace falta una de verdad porque auditoria_actor_coherente exige que todo lo
// que no sea el sistema tenga actor_id, y esa columna apunta a plataforma.cuenta.
// Es administrador del tenant de pruebas: el CHECK cuenta_alcance_por_tipo no
// admite un administrador sin tenant, que es RF-23 hecho estructura.
//
// El correo tiene que ser único —el índice no admite repetidos y estas filas no
// se borran— y por eso lleva un sufijo aleatorio.
func cuentaAdministradora(t *testing.T, bd *datos.BD) string {
	t.Helper()

	var id string
	err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO plataforma.cuenta
				(nombre, email, tipo, tenant_id, estado, email_verificado)
			VALUES ($1, $2, 'administrador', $3, 'activa', true)
			RETURNING id::text`,
			"Administrador de pruebas",
			"config-"+uuid.NewString()[:8]+"@ejemplo.test",
			pruebas.Tenant).Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo crear la cuenta administradora: %v", err)
	}

	return id
}
