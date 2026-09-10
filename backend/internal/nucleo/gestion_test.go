package nucleo_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Reprogramar (RF-07), registrar una transición manual (RF-28 / RF-32) y
// calificar (RF-20).
//
// El lunes es el día de este paquete; estas pruebas usan la tarde —de 13 a 17—
// para no pisarse con las de creación y cancelación, que usan la mañana.

func actorAdmin(t *testing.T, bd *datos.BD) auditoria.Actor {
	t.Helper()

	return auditoria.Actor{Tipo: "administrador", ID: cuentaDePrueba(t, bd)}
}

// ------------------------------------------------------------------ RF-07 --

// Mover una reserva libera el cupo viejo y toma el nuevo en la MISMA
// transacción. No hay un instante intermedio en el que la persona se quede sin
// ninguno de los dos, que es exactamente lo que tendría un cancelar-y-crear.
func TestModificarLiberaElCupoVieroYTomaElNuevo(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	viejo, viejoFin := pruebas.Lunes(13), pruebas.Lunes(14)
	nuevo, nuevoFin := pruebas.Lunes(14), pruebas.Lunes(15)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, viejo, nuevoFin)

	id := reservaEn(t, svc, viejo, viejoFin)

	alcance := dominio.Alcance{Destino: correoPropio}
	movida, err := svc.Modificar(t.Context(), tenant, id, alcance, auditoria.Actor{},
		api.ModificacionReserva{Periodo: api.Periodo{Inicio: nuevo, Fin: nuevoFin}})
	if err != nil {
		t.Fatalf("Modificar devolvió error: %v", err)
	}

	if !movida.Periodo.Inicio.Equal(nuevo) {
		t.Fatalf("la reserva quedó en %s y se pidió %s", movida.Periodo.Inicio, nuevo)
	}
	if movida.Id != id {
		t.Fatal("se creó una reserva nueva en vez de mover la que había")
	}

	// El cupo viejo quedó libre: otra reserva cabe ahí.
	otra := peticion(clave(t), viejo, viejoFin)
	if _, err := svc.Crear(t.Context(), otra); err != nil {
		t.Fatalf("el horario viejo no se liberó: %v", err)
	}

	// Y el nuevo está ocupado por la que se movió.
	if _, err := svc.Crear(t.Context(), peticion(clave(t), nuevo, nuevoFin)); !errors.Is(
		err, datos.ErrHorarioOcupado) {
		t.Fatalf("el horario nuevo no quedó ocupado: %v", err)
	}
}

// El horario nuevo lo arbitra el motor, igual que al crear: no se comprueba
// disponibilidad y luego se mueve —eso sería una carrera con una ventana entre
// las dos consultas—, se mueve y se traduce el rechazo.
func TestModificarHaciaUnCupoTomadoDaHorarioOcupado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	mia, miaFin := pruebas.Lunes(15), pruebas.Lunes(16)
	ajena, ajenaFin := pruebas.Lunes(16), pruebas.Lunes(17)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, mia, ajenaFin)

	id := reservaEn(t, svc, mia, miaFin)
	reservaEn(t, svc, ajena, ajenaFin)

	_, err := svc.Modificar(t.Context(), tenant, id,
		dominio.Alcance{Destino: correoPropio}, auditoria.Actor{},
		api.ModificacionReserva{Periodo: api.Periodo{Inicio: ajena, Fin: ajenaFin}})
	if !errors.Is(err, datos.ErrHorarioOcupado) {
		t.Fatalf("se pudo mover encima de otra reserva: %v", err)
	}

	// Y la reserva se quedó donde estaba: la transacción revierte entera.
	var inicio time.Time
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT lower(periodo) FROM negocio.reserva WHERE id = $1", id).Scan(&inicio)
	}); err != nil {
		t.Fatalf("no se pudo releer la reserva: %v", err)
	}
	if !inicio.Equal(mia) {
		t.Fatalf("la reserva se movió a %s pese a que la operación falló", inicio)
	}
}

// La reserva de otra persona responde igual que una inexistente. Distinguirlas
// confirmaría qué identificadores son reales.
func TestNoSeModificaLaReservaDeOtro(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(13), pruebas.Lunes(14)
	destino, destinoFin := pruebas.Lunes(14), pruebas.Lunes(15)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, destinoFin)

	id := reservaEn(t, svc, inicio, fin)

	_, err := svc.Modificar(t.Context(), tenant, id,
		dominio.Alcance{Destino: "intruso@ejemplo.test"}, auditoria.Actor{},
		api.ModificacionReserva{Periodo: api.Periodo{Inicio: destino, Fin: destinoFin}})
	if !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se pudo mover la reserva de otra persona: %v", err)
	}
}

// La duración la fija el servicio y no cambia al reprogramar: el precio se
// congeló sobre ella, así que una cita de otra duración al mismo precio sería
// otra cosa vendida como la misma.
func TestModificarNoPuedeCambiarLaDuracion(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(13), pruebas.Lunes(14)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, pruebas.Lunes(16))

	id := reservaEn(t, svc, inicio, fin)

	_, err := svc.Modificar(t.Context(), tenant, id,
		dominio.Alcance{Destino: correoPropio}, auditoria.Actor{},
		api.ModificacionReserva{
			Periodo: api.Periodo{Inicio: pruebas.Lunes(14), Fin: pruebas.Lunes(16)},
		})
	if !errors.Is(err, dominio.ErrDuracionNoCoincide) {
		t.Fatalf("se aceptó una reprogramación de otra duración: %v", err)
	}
}

// ------------------------------------------------------- RF-28 y RF-32 --

// El check-in que faltaba: sin él, una cita a la que alguien sí llegó acababa
// en `no_show` por umbral, porque nada la movía.
func TestElCheckInMueveAEnCursoYDejaHistoria(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(13), pruebas.Lunes(14)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)
	confirmar(t, bd, id)

	admin := actorAdmin(t, bd)
	alcance := dominio.Alcance{TenantCompleto: true, Cuenta: admin.ID}

	reserva, err := svc.CambiarEstado(t.Context(), tenant, id, alcance, admin,
		api.CambioEstado{Estado: api.EnCurso})
	if err != nil {
		t.Fatalf("CambiarEstado devolvió error: %v", err)
	}
	if reserva.Estado != api.EnCurso {
		t.Fatalf("la reserva quedó en %q", reserva.Estado)
	}

	var actorTipo, actorID string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT actor_tipo::text, actor_id::text
			FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_nuevo = 'en_curso'`, id).Scan(&actorTipo, &actorID)
	}); err != nil {
		t.Fatalf("no se pudo leer la transición: %v", err)
	}
	if actorTipo != "administrador" || actorID != admin.ID {
		t.Fatalf("la transición quedó como %q/%s", actorTipo, actorID)
	}

	// Y de en_curso se puede cerrar la cita.
	if _, err := svc.CambiarEstado(t.Context(), tenant, id, alcance, admin,
		api.CambioEstado{Estado: api.Completada}); err != nil {
		t.Fatalf("no se pudo completar la cita: %v", err)
	}
}

// La tabla de transiciones de RF-28 es cerrada: lo que no sale del estado
// actual se rechaza diciendo en cuál está, porque "no se puede" a secas obliga
// a la agenda a recargar y comparar.
func TestUnaTransicionQueNoSaleDelEstadoActualSeRechaza(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(14), pruebas.Lunes(15)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin) // nace pendiente
	admin := actorAdmin(t, bd)
	alcance := dominio.Alcance{TenantCompleto: true, Cuenta: admin.ID}

	// De pendiente no se hace check-in: primero tiene que confirmarse el pago
	// (RF-33). Es la leyenda de RF-28 escrita como tabla.
	_, err := svc.CambiarEstado(t.Context(), tenant, id, alcance, admin,
		api.CambioEstado{Estado: api.EnCurso})
	if !errors.Is(err, nucleo.ErrNoCancelable) {
		t.Fatalf("se aceptó un check-in sobre una reserva pendiente: %v", err)
	}

	var invalida nucleo.ErrTransicionInvalida
	if !errors.As(err, &invalida) {
		t.Fatalf("el error no dice de qué estado a cuál: %v", err)
	}
	if invalida.Desde != api.Pendiente || invalida.Hasta != api.EnCurso {
		t.Fatalf("el error dice %q -> %q", invalida.Desde, invalida.Hasta)
	}
}

// Registrar una transición es de la agenda del NEGOCIO, no del cliente, aunque
// la reserva sea suya.
func TestUnClienteNoRegistraSuPropioCheckIn(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(15), pruebas.Lunes(16)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)
	confirmar(t, bd, id)

	_, err := svc.CambiarEstado(t.Context(), tenant, id,
		dominio.Alcance{Destino: correoPropio}, auditoria.Actor{},
		api.CambioEstado{Estado: api.EnCurso})
	if !errors.Is(err, dominio.ErrSinAlcance) {
		t.Fatalf("un cliente registró su propio check-in: %v", err)
	}
}

// ------------------------------------------------------------------ RF-20 --

func TestSoloSeCalificaUnaReservaCompletadaYUnaSolaVez(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(16), pruebas.Lunes(17)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)
	alcance := dominio.Alcance{Destino: correoPropio}
	comentario := "todo bien"

	// Todavía no ocurrió: calificarla sería una expectativa, no una opinión.
	if _, err := svc.Calificar(t.Context(), tenant, id, alcance,
		api.NuevaCalificacion{Puntaje: 5},
	); !errors.Is(err, nucleo.ErrNoCalificable) {
		t.Fatalf("se calificó una reserva pendiente: %v", err)
	}

	completar(t, bd, id)

	calificacion, err := svc.Calificar(t.Context(), tenant, id, alcance,
		api.NuevaCalificacion{Puntaje: 4, Comentario: &comentario})
	if err != nil {
		t.Fatalf("Calificar devolvió error: %v", err)
	}
	if calificacion.Puntaje != 4 || calificacion.ReservaId != id {
		t.Fatalf("la calificación salió como %+v", calificacion)
	}

	// Una por reserva, y la arbitra el índice único: dos envíos simultáneos del
	// mismo formulario atravesarían una comprobación previa.
	if _, err := svc.Calificar(t.Context(), tenant, id, alcance,
		api.NuevaCalificacion{Puntaje: 1},
	); !errors.Is(err, nucleo.ErrYaCalificada) {
		t.Fatalf("se calificó dos veces la misma reserva: %v", err)
	}

	// Y la reserva de otra persona no se califica.
	if _, err := svc.Calificar(t.Context(), tenant, id,
		dominio.Alcance{Destino: "intruso@ejemplo.test"}, api.NuevaCalificacion{Puntaje: 1},
	); !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se calificó la reserva de otra persona: %v", err)
	}
}

// ------------------------------------------------------------ auxiliares --

// confirmar y completar mueven el estado con SQL directo.
//
// A propósito, y no llamando a CambiarEstado: estas pruebas necesitan LLEGAR a
// un estado para comprobar otra cosa, y usar el código que se está probando
// para preparar el escenario haría que un fallo suyo se pareciera a un fallo de
// lo que se quería comprobar.
//
// La transición se escribe igual, porque transicion_estado no admite huecos: la
// historia de una reserva tiene que poder leerse entera.
func confirmar(t *testing.T, bd *datos.BD, id uuid.UUID) {
	t.Helper()
	moverEstado(t, bd, id, "pendiente", "confirmada")
}

func completar(t *testing.T, bd *datos.BD, id uuid.UUID) {
	t.Helper()
	moverEstado(t, bd, id, "pendiente", "confirmada")
	moverEstado(t, bd, id, "confirmada", "en_curso")
	moverEstado(t, bd, id, "en_curso", "completada")
}

func moverEstado(t *testing.T, bd *datos.BD, id uuid.UUID, desde, hasta string) {
	t.Helper()

	err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `
			UPDATE negocio.reserva
			SET estado = $2::negocio.estado_reserva,
			    expira_en = CASE WHEN $2 = 'confirmada' THEN NULL ELSE expira_en END
			WHERE id = $1`, id, hasta); err != nil {
			return err
		}

		_, err := tx.Exec(t.Context(), `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
			VALUES ($1, $2, $3::negocio.estado_reserva, $4::negocio.estado_reserva,
			        'sistema', 'preparación de la prueba')`,
			pruebas.Tenant, id, desde, hasta)
		return err
	})
	if err != nil {
		t.Fatalf("no se pudo mover la reserva a %q: %v", hasta, err)
	}
}
