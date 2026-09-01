package nucleo_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Cancelación (RF-06). El correo de contacto es la autorización: sin RF-12 es
// lo único que identifica a quien reservó como invitado.
const correoPropio = "nucleo@ejemplo.test"

// reservaEn crea una reserva y devuelve su identificador.
func reservaEn(t *testing.T, svc *nucleo.Servicio, inicio, fin time.Time) uuid.UUID {
	t.Helper()

	reserva, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
	if err != nil {
		t.Fatalf("no se pudo crear la reserva de la prueba: %v", err)
	}
	return reserva.Id
}

func TestCancelarLiberaElCupoYDejaHistoria(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	// El lunes, el día de este paquete. Reutilizar las mismas horas que
	// nucleo_test es seguro: dentro de un paquete las pruebas corren en serie y
	// cada una limpia su franja antes de empezar. Lo que no se puede compartir
	// es el DÍA con otro paquete, porque esos sí corren en paralelo.
	inicio, fin := pruebas.Lunes(9), pruebas.Lunes(10)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)

	reserva, err := svc.Cancelar(t.Context(), tenant, id, correoPropio)
	if err != nil {
		t.Fatalf("Cancelar devolvió error: %v", err)
	}
	if reserva.Estado != api.Cancelada {
		t.Fatalf("estado %q tras cancelar", reserva.Estado)
	}

	// expira_en se limpia: una reserva cancelada ya no es un bloqueo, y dejarlo
	// puesto haría que el expirador de RF-27 la mirase para siempre.
	if reserva.ExpiraEn != nil {
		t.Errorf("la reserva cancelada conserva expira_en (%s)", reserva.ExpiraEn)
	}

	// El cupo queda libre: una cancelada sale del predicado de la restricción
	// EXCLUDE, así que la misma franja se puede volver a reservar. Es la
	// comprobación que de verdad importa, porque cancelar sin liberar sería
	// perder inventario en silencio.
	if _, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin)); err != nil {
		t.Fatalf("la franja no quedó libre tras cancelar: %v", err)
	}

	// Y la historia de RF-28 quedó escrita.
	var transiciones int
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT count(*) FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_nuevo = 'cancelada'`, id).Scan(&transiciones)
	}); err != nil {
		t.Fatalf("no se pudo verificar la transición: %v", err)
	}
	if transiciones != 1 {
		t.Errorf("se escribieron %d transiciones a cancelada; se esperaba 1", transiciones)
	}
}

// Cancelar dos veces no vuelve a cancelar: la segunda choca con el estado.
func TestCancelarDosVecesDaNoCancelable(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(10), pruebas.Lunes(11)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)

	if _, err := svc.Cancelar(t.Context(), tenant, id, correoPropio); err != nil {
		t.Fatalf("la primera cancelación debía funcionar: %v", err)
	}

	if _, err := svc.Cancelar(t.Context(), tenant, id, correoPropio); !errors.Is(err, nucleo.ErrNoCancelable) {
		t.Fatalf("se esperaba ErrNoCancelable, se obtuvo %v", err)
	}
}

// La reserva de otra persona no se puede cancelar, y responde como si no
// existiera. Un error distinto confirmaría que ese identificador es real.
func TestNoSeCancelaLaReservaDeOtro(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	inicio, fin := pruebas.Lunes(11), pruebas.Lunes(12)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaEn(t, svc, inicio, fin)

	_, err := svc.Cancelar(t.Context(), tenant, id, "intruso@ejemplo.test")
	if !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se esperaba ErrNoEncontrado, se obtuvo %v", err)
	}

	// Y sigue viva: el intento no la tocó.
	var estado string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT estado::text FROM negocio.reserva WHERE id = $1", id).Scan(&estado)
	}); err != nil {
		t.Fatalf("no se pudo leer la reserva: %v", err)
	}
	if estado != "pendiente" {
		t.Fatalf("la reserva quedó en %q tras el intento ajeno", estado)
	}
}

// La política que se aplica es la que la reserva CONGELÓ, no la vigente hoy
// (RF-15). La semilla exige 24 horas de antelación, así que una cita que empieza
// dentro de una hora ya no se puede cancelar.
func TestFueraDePlazoNoSeCancela(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	// Se inserta directamente: crear por la ruta normal exige que la cita esté
	// en el futuro y dentro del horario, y lo que hace falta aquí es una cita
	// inminente, que es justo lo que esa ruta impide.
	inicio := time.Now().Add(time.Hour)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	var id uuid.UUID
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		var texto string
		if err := tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Inminente', $6,
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'confirmada', NULL,
				80000.00, 'COP', $7
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, inicio.Add(time.Hour), correoPropio, pruebas.Politica).Scan(&texto); err != nil {
			return err
		}
		var err error
		id, err = uuid.Parse(texto)
		return err
	}); err != nil {
		t.Fatalf("no se pudo sembrar la reserva inminente: %v", err)
	}

	// La limpieza de arriba ya devuelve el cupo al terminar. No se usa
	// t.Context() dentro de un Cleanup: Go lo cancela ANTES de ejecutar los
	// cleanups, así que una limpieza escrita así falla en silencio y la fila
	// sobrevive hasta la siguiente ejecución, que revienta con un 23P01 sin
	// relación con lo que probaba.

	_, err := svc.Cancelar(t.Context(), tenant, id, correoPropio)
	if !errors.Is(err, nucleo.ErrFueraDePlazo) {
		t.Fatalf("se esperaba ErrFueraDePlazo, se obtuvo %v", err)
	}

	// El mensaje lleva las horas que pedía la política. "No se puede" a secas
	// obligaría a la persona a adivinar por qué.
	var plazo nucleo.ErrPlazoVencido
	if !errors.As(err, &plazo) {
		t.Fatalf("el error no lleva el detalle del plazo: %v", err)
	}
	if plazo.HorasRequeridas != 24 {
		t.Errorf("la política sembrada pide 24 horas y el error dice %d", plazo.HorasRequeridas)
	}
}
