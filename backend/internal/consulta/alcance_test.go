package consulta_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// El alcance de RF-23 sobre la ruta de lectura.
//
// La misma llamada devuelve conjuntos distintos según quién la haga, y eso es
// lo que RF-23 modela: una operación con el alcance acotado por la cuenta, no
// tres rutas por rol. Estas pruebas fijan los tres conjuntos.
//
// La franja es la tarde del martes —14, 15 y 16— para no pisarse con sembrar(),
// que usa de 9 a 14. La semilla abre de 9 a 17 locales, así que las tres caben.

// cuentaDePrueba crea una cuenta y devuelve su identificador.
//
// Se crea con SQL directo y no con internal/identidad porque lo que se prueba
// aquí es la LECTURA: meter el alta de RF-24 en medio ataría estas pruebas a un
// paquete que no tienen por qué conocer, y las haría fallar por motivos que no
// son suyos.
func cuentaDePrueba(t *testing.T, bd *datos.BD, correo string) string {
	t.Helper()

	var id string
	err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO plataforma.cuenta (nombre, email, tipo, estado, email_verificado)
			VALUES ($1, $2, 'usuario', 'activa', true)
			RETURNING id::text`, "Alcance De Prueba", correo).Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo crear la cuenta de prueba: %v", err)
	}

	return id
}

// reservaEn inserta una reserva en la franja indicada, con o sin cuenta.
func reservaEn(t *testing.T, bd *datos.BD, inicio time.Time, cuentaID, correo string) string {
	t.Helper()

	var id string
	err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id, cuenta_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, NULLIF($7, '')::uuid, 'Alcance', $8,
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'pendiente', now() + interval '1 hour',
				80000.00, 'COP', $6
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, inicio.Add(time.Hour), pruebas.Politica, cuentaID, correo).Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo insertar la reserva de prueba: %v", err)
	}

	return id
}

func contiene(lista api.ListaReservas, id string) bool {
	for _, reserva := range lista.Datos {
		if reserva.Id.String() == id {
			return true
		}
	}
	return false
}

// escenario deja tres reservas en la tarde del martes: una de la cuenta, una de
// invitado con el MISMO correo que esa cuenta, y una de otra persona.
func escenario(t *testing.T, bd *datos.BD) (cuentaID, correo, dePropia, deInvitado, deOtro string, f consulta.Filtro) {
	t.Helper()

	desde, hasta := pruebas.Martes(14), pruebas.Martes(17)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, desde, hasta)

	correo = "alcance-" + uuid.NewString()[:8] + "@ejemplo.test"
	cuentaID = cuentaDePrueba(t, bd, correo)

	dePropia = reservaEn(t, bd, pruebas.Martes(14), cuentaID, correo)
	deInvitado = reservaEn(t, bd, pruebas.Martes(15), "", correo)
	deOtro = reservaEn(t, bd, pruebas.Martes(16), "", "ajeno-"+uuid.NewString()[:8]+"@ejemplo.test")

	f = consulta.Filtro{
		Estados: []api.EstadoReserva{api.Pendiente},
		Desde:   &desde,
		Hasta:   &hasta,
	}
	return cuentaID, correo, dePropia, deInvitado, deOtro, f
}

// Una cuenta ve lo suyo Y lo que reservó como invitado con su correo ya
// verificado. Es lo que RF-24 llama "asociar las reservas previas", resuelto en
// la lectura porque negocio.reserva está particionada por tenant y no hay
// índice global de reservas de invitado que permita barrerlas.
func TestUnaCuentaVeLoSuyoYLoQueReservoComoInvitado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	cuentaID, correo, dePropia, deInvitado, deOtro, filtro := escenario(t, bd)

	lista, err := svc.Listar(t.Context(), tenant,
		consulta.Alcance{Cuenta: cuentaID, Destino: correo}, filtro)
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}

	if !contiene(lista, dePropia) {
		t.Error("no se ve la reserva hecha con la cuenta")
	}
	if !contiene(lista, deInvitado) {
		t.Error("no se ve la reserva hecha como invitado con el mismo correo (RF-24)")
	}
	if contiene(lista, deOtro) {
		t.Error("se ve la reserva de otra persona")
	}
}

// Sin el correo verificado, una cuenta ve SOLO lo suyo. Es la diferencia que
// impide que escribir la dirección de otra persona en el perfil baste para
// heredar sus reservas de invitado: hasta que se verifica, el correo no viaja
// en el token y por tanto no acredita nada.
func TestSinCorreoVerificadoLaCuentaNoHeredaLoDeInvitado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	cuentaID, _, dePropia, deInvitado, _, filtro := escenario(t, bd)

	lista, err := svc.Listar(t.Context(), tenant, consulta.Alcance{Cuenta: cuentaID}, filtro)
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}

	if !contiene(lista, dePropia) {
		t.Error("no se ve la reserva de la propia cuenta")
	}
	if contiene(lista, deInvitado) {
		t.Error("se heredaron reservas de invitado con un correo sin verificar")
	}
}

// El administrador ve el tenant entero (RF-32). No hace falta decirle cuál: la
// transacción ya corre con su tenant fijado y RLS no deja ver otro.
func TestElAdministradorVeElTenantEntero(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	_, _, dePropia, deInvitado, deOtro, filtro := escenario(t, bd)

	lista, err := svc.Listar(t.Context(), tenant, consulta.Alcance{TenantCompleto: true}, filtro)
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}

	for _, id := range []string{dePropia, deInvitado, deOtro} {
		if !contiene(lista, id) {
			t.Errorf("el administrador no ve la reserva %s de su propio tenant", id)
		}
	}
}

// Los filtros de la agenda de RF-32. Se comprueban con el alcance del
// administrador porque es quien los usa, pero no dependen de él: acotan, no
// autorizan.
func TestLaAgendaSeFiltraPorSedeYPorRecurso(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	_, _, dePropia, _, _, filtro := escenario(t, bd)

	recurso := uuid.MustParse(pruebas.Recurso)
	sede := uuid.MustParse(pruebas.Sede)
	otroID := uuid.New()

	conRecurso := filtro
	conRecurso.Recurso = &recurso
	lista, err := svc.Listar(t.Context(), tenant, consulta.Alcance{TenantCompleto: true}, conRecurso)
	if err != nil {
		t.Fatalf("Listar por recurso devolvió error: %v", err)
	}
	if !contiene(lista, dePropia) {
		t.Error("el filtro por el recurso correcto dejó fuera una reserva suya")
	}

	conSede := filtro
	conSede.Sede = &sede
	lista, err = svc.Listar(t.Context(), tenant, consulta.Alcance{TenantCompleto: true}, conSede)
	if err != nil {
		t.Fatalf("Listar por sede devolvió error: %v", err)
	}
	if !contiene(lista, dePropia) {
		t.Error("el filtro por la sede correcta dejó fuera una reserva suya")
	}

	// Y un recurso que no es el suyo no devuelve nada de esa franja.
	conOtroRecurso := filtro
	conOtroRecurso.Recurso = &otroID
	lista, err = svc.Listar(t.Context(), tenant, consulta.Alcance{TenantCompleto: true}, conOtroRecurso)
	if err != nil {
		t.Fatalf("Listar por otro recurso devolvió error: %v", err)
	}
	if len(lista.Datos) != 0 {
		t.Errorf("filtrar por un recurso ajeno devolvió %d reservas", len(lista.Datos))
	}
}

// Obtener sigue el mismo alcance que Listar, y tiene que seguirlo: si el
// detalle fuera más permisivo que el listado, bastaría con adivinar un
// identificador para saltarse la lista.
func TestElDetalleUsaElMismoAlcanceQueElListado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	cuentaID, correo, _, _, deOtro, _ := escenario(t, bd)

	if _, err := svc.Obtener(
		t.Context(), tenant, consulta.Alcance{Cuenta: cuentaID, Destino: correo},
		uuid.MustParse(deOtro),
	); err == nil {
		t.Fatal("una cuenta pudo ver el detalle de la reserva de otra persona")
	}

	if _, err := svc.Obtener(
		t.Context(), tenant, consulta.Alcance{TenantCompleto: true}, uuid.MustParse(deOtro),
	); err != nil {
		t.Fatalf("el administrador no pudo ver una reserva de su tenant: %v", err)
	}
}
