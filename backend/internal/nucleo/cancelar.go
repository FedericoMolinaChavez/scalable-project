package nucleo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/reservas"
)

// Cancelación de una reserva (RF-06).
//
// Vive en el núcleo por la misma razón que la creación: escribe en
// negocio.reserva, y esa tabla tiene un solo dueño por la ruta síncrona.
// Cancelar además LIBERA cupo —una reserva cancelada sale del predicado de la
// restricción EXCLUDE— así que es exactamente igual de delicado que crearla,
// solo que en el otro sentido.

var (
	// ErrNoCancelable: la reserva ya no está en un estado desde el que se
	// pueda cancelar. Es distinto de "fuera de plazo": aquí no hay nada que
	// cancelar, no es que no se permita.
	ErrNoCancelable = errors.New("la reserva ya no se puede cancelar en su estado actual")

	// ErrFueraDePlazo: la política congelada en la reserva ya no lo permite.
	ErrFueraDePlazo = errors.New("el plazo de cancelación de esta reserva ya pasó")
)

// ErrPlazoVencido lleva el detalle que la interfaz necesita para explicarlo.
//
// Un "no se puede cancelar" a secas obliga a la persona a adivinar por qué, y
// la respuesta correcta —"tenías hasta 24 horas antes"— está a un campo de
// distancia en la política que la reserva ya guarda.
type ErrPlazoVencido struct {
	HorasRequeridas int
	HorasRestantes  float64
}

func (e ErrPlazoVencido) Error() string {
	return fmt.Sprintf(
		"esta reserva se podía cancelar hasta %d horas antes, y ya faltan menos de %.0f",
		e.HorasRequeridas, e.HorasRestantes)
}

func (e ErrPlazoVencido) Is(objetivo error) bool { return objetivo == ErrFueraDePlazo }

// estadosCancelables son los únicos desde los que RF-28 permite ir a cancelada.
//
// No incluye en_curso ni completada: una cita que ya empezó no se cancela, se
// resuelve de otra forma (no_show es del negocio, no del cliente). Tampoco
// expirada ni cancelada, que ya no ocupan cupo.
var estadosCancelables = map[api.EstadoReserva]bool{
	api.Pendiente:  true,
	api.Confirmada: true,
}

// Cancelar aplica la cancelación y devuelve la reserva ya cancelada.
//
// `destino` es el correo que el token acreditó. La reserva solo se cancela si
// es suya, y esa comprobación va DENTRO de la misma transacción que escribe:
// comprobarla antes dejaría una ventana en la que la reserva cambia de manos
// —o de estado— entre el permiso y el efecto.
func (s *Servicio) Cancelar(
	ctx context.Context, tenant uuid.UUID, reservaID uuid.UUID, destino string,
) (api.Reserva, error) {
	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var (
			estado           string
			inicio           time.Time
			horasCancelacion int

			// La penalidad de la política CONGELADA en la reserva, no la
			// vigente hoy (RF-15). Es lo que impide que un negocio endurezca su
			// política el martes y se la cobre a quien reservó el lunes.
			// Nullable: sin penalidad se devuelve todo.
			penalidadPct *float64
		)

		// FOR UPDATE sobre la reserva: dos cancelaciones simultáneas de la
		// misma fila tienen que serializarse, o las dos leen 'confirmada', las
		// dos deciden que se puede, y se escriben dos transiciones para un solo
		// cambio.
		//
		// El filtro por contacto_email es la autorización, y por eso va en el
		// WHERE y no en un `if` posterior: una reserva de otra persona
		// devuelve cero filas, igual que una que no existe. Desde fuera son
		// indistinguibles, que es justo lo que evita usar identificadores
		// ajenos para comprobar cuáles son reales.
		err := tx.QueryRow(ctx, `
			SELECT r.estado::text, lower(r.periodo),
			       p.rango_cancelacion_horas, p.penalidad_pct
			FROM negocio.reserva r
			JOIN negocio.politica_version p
			  ON p.tenant_id = r.tenant_id AND p.id = r.politica_version_id
			WHERE r.id = $1
			  AND r.cuenta_id IS NULL
			  AND lower(r.contacto_email) = $2
			FOR UPDATE OF r`,
			reservaID, destino,
		).Scan(&estado, &inicio, &horasCancelacion, &penalidadPct)
		if errors.Is(err, pgx.ErrNoRows) {
			return datos.ErrNoEncontrado
		}
		if err != nil {
			return err
		}

		if !estadosCancelables[api.EstadoReserva(estado)] {
			return ErrNoCancelable
		}

		// El plazo se mide contra el INICIO de la cita, no contra ahora mismo
		// ni contra la fecha de creación: "hasta 24 horas antes" habla de la
		// cita, que es lo que el negocio pierde si nadie la ocupa.
		restantes := time.Until(inicio).Hours()
		if restantes < float64(horasCancelacion) {
			return ErrPlazoVencido{
				HorasRequeridas: horasCancelacion,
				HorasRestantes:  restantes,
			}
		}

		if _, err := tx.Exec(ctx, `
			UPDATE negocio.reserva
			SET estado = 'cancelada', expira_en = NULL
			WHERE id = $1`, reservaID); err != nil {
			return err
		}

		// La historia se escribe en la misma transacción que el cambio (RF-28).
		// actor_tipo 'sistema' porque transicion_actor_coherente exige que solo
		// el sistema tenga actor_id nulo, y un invitado no tiene cuenta que
		// poner ahí. Con RF-12 esto pasa a 'usuario' con su identificador.
		if _, err := tx.Exec(ctx, `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
			VALUES ($1, $2, $3::negocio.estado_reserva, 'cancelada', 'sistema', $4)`,
			tenant, reservaID, estado,
			"cancelada por quien reservó, identificado con un código (RF-06)"); err != nil {
			return err
		}

		// El reembolso de RF-29, ANOTADO y no ejecutado.
		//
		// Va en esta misma transacción porque es la que sabe que el dinero hay
		// que devolverlo: fuera de ella, un fallo entre la cancelación y la
		// anotación dejaría una reserva cancelada y un cobro que nadie sabe que
		// debe volver. Lo que NO ocurre aquí es la llamada a Stripe: eso lo hace
		// el trabajador de reembolsos, con sus reintentos, porque una petición
		// HTTP no puede quedarse esperando a un tercero.
		//
		// Por eso la respuesta de esta ruta no dice nada del dinero, tal como
		// declara el contrato: en este instante todavía no se ha movido.
		if err := anotarReembolso(ctx, tx, reservaID, penalidadPct); err != nil {
			return err
		}

		reserva, err = reservas.Escanear(tx.QueryRow(ctx,
			`SELECT `+reservas.Columnas+` FROM negocio.reserva WHERE id = $1`, reservaID))
		if err != nil {
			return err
		}

		// El evento va en esta misma transacción, igual que al crear: cancelar
		// libera cupo y eso es tan digno de avisar como ocuparlo.
		return emitir(ctx, tx, tenant, EventoReservaCancelada, reserva)
	})
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}

// anotarReembolso deja escrito que hay que devolver el dinero de una reserva
// que se acaba de cancelar (RF-29).
//
// Tres decisiones caben en esta consulta y conviene verlas por separado.
//
// La primera es que solo mira pagos CONFIRMADOS. Una reserva cancelada mientras
// estaba pendiente no tiene nada que devolver: su intento de cobro se cierra
// solo cuando el conciliador lo alcanza, o nunca llegó a cobrar. Anotar un
// reembolso ahí crearía una fila que el trabajador intentaría cinco veces
// contra un cobro que no existe.
//
// La segunda es la penalidad. La política que se aplica es la que la reserva
// CONGELÓ al crearse (RF-15), no la vigente hoy, y si lleva penalidad_pct el
// negocio se queda esa parte: se devuelve el resto. Con penalidad del 100% no
// se anota nada, porque un reembolso de cero no es un reembolso.
//
// La tercera es el ON CONFLICT, que es la idempotencia de RF-29 traída del
// esquema: una fila por pago. Cancelar dos veces —o cancelar algo que el
// webhook ya había mandado devolver por sin_cupo— no duplica la devolución.
func anotarReembolso(
	ctx context.Context, tx pgx.Tx, reservaID uuid.UUID, penalidadPct *float64,
) error {
	// Sin penalidad se devuelve el importe completo. El cálculo se hace en SQL
	// sobre la columna numeric y no en Go: pasarlo por un float64 introduciría
	// justo el error de redondeo que el esquema y el contrato se han cuidado de
	// evitar en todo lo demás.
	retencion := 0.0
	if penalidadPct != nil {
		retencion = *penalidadPct
	}
	if retencion >= 100 {
		return nil
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO negocio.reembolso (tenant_id, pago_id, monto, motivo, estado)
		SELECT p.tenant_id, p.id,
		       round(p.monto * (100 - $2::numeric) / 100, 2),
		       'cancelacion_usuario', 'pendiente'
		FROM negocio.pago p
		WHERE p.reserva_id = $1
		  AND p.estado = 'confirmado'
		  AND round(p.monto * (100 - $2::numeric) / 100, 2) > 0
		ON CONFLICT (tenant_id, pago_id) DO NOTHING`,
		reservaID, retencion)
	return err
}
