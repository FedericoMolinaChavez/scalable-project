package nucleo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/reservas"
)

// Las transiciones MANUALES de RF-28, las que la agenda del administrador
// dispara (RF-32).
//
// La leyenda de RF-28 reparte quién mueve cada estado, y el reparto no es
// decorativo: las automáticas —expirar una pendiente, cerrar una cita pasada,
// marcar una ausencia por umbral— las hace el trabajador porque dependen del
// reloj y de nadie más. Estas tres dependen de que alguien mire y decida:
//
//	confirmada -> en_curso     el check-in: la persona llegó
//	confirmada -> no_show      la persona no llegó, y se registra antes de que
//	                           el umbral lo haga solo
//	en_curso   -> completada   la cita terminó
//
// Cancelar NO está aquí aunque también sea una transición, y la diferencia es
// que libera cupo y aplica una política: tiene su propia ruta, su propio plazo
// y su propio 422.
//
// El backend decía "el sistema no marca la llegada de nadie" y era cierto: sin
// esta superficie, una cita a la que alguien sí llegó acababa en `no_show` por
// umbral. Esto es lo que faltaba para que ese estado signifique lo que dice.

// ErrTransicionInvalida: esa transición no sale del estado actual.
//
// Lleva los dos estados dentro porque la agenda necesita poder decir por qué su
// botón no hizo nada: "no se puede" a secas obliga a recargar y comparar.
type ErrTransicionInvalida struct {
	Desde api.EstadoReserva
	Hasta api.EstadoReserva
}

func (e ErrTransicionInvalida) Error() string {
	return fmt.Sprintf("la reserva está en %q y desde ahí no se puede pasar a %q",
		e.Desde, e.Hasta)
}

// Is la hace comparable con ErrNoCancelable, que es el mismo tipo de conflicto
// —el recurso no está en un estado que admita la operación— y por tanto el
// mismo 409. Sin esto, internal/rutas necesitaría una rama más para decir
// exactamente lo mismo.
func (e ErrTransicionInvalida) Is(objetivo error) bool { return objetivo == ErrNoCancelable }

// transicionesManuales es RF-28 escrito como lo que es: una tabla de qué sale
// de dónde.
//
// En una tabla y no en una cadena de `if` porque la pregunta que responde
// —"¿es válida esta transición?"— es exactamente una consulta a un conjunto, y
// porque así añadir una transición nueva es añadir una entrada, no encontrar el
// `if` correcto entre varios que se parecen.
var transicionesManuales = map[api.EstadoReserva]map[api.EstadoReserva]string{
	api.Confirmada: {
		api.EnCurso: "el administrador registró la llegada (RF-32)",
		api.NoShow:  "el administrador registró la ausencia (RF-32)",
	},
	api.EnCurso: {
		api.Completada: "el administrador cerró la cita (RF-32)",
	},
}

// CambiarEstado aplica una transición manual (RF-28, RF-32).
//
// El alcance tiene que ser el del tenant: son acciones de la agenda del
// negocio, no del cliente. Quien las pide sin ser administrador no recibe un
// 404 sino un rechazo de alcance, porque aquí no hay nada que ocultar: la
// reserva es suya, lo que no es suyo es el gesto.
func (s *Servicio) CambiarEstado(
	ctx context.Context, tenant, reservaID uuid.UUID,
	alcance dominio.Alcance, actor auditoria.Actor,
	cambio api.CambioEstado,
) (api.Reserva, error) {
	if !alcance.TenantCompleto {
		return api.Reserva{}, dominio.ErrSinAlcance
	}

	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var actual string

		// FOR UPDATE: dos check-ins simultáneos sobre la misma reserva tienen
		// que serializarse, o los dos leen 'confirmada', los dos deciden que se
		// puede, y se escriben dos transiciones para un solo cambio.
		//
		// Sin filtro de alcance en el WHERE, al revés que en cancelar: aquí ya
		// se comprobó que quien pide administra este tenant, y RLS acota el
		// resto. Añadir el filtro no cambiaría nada y sugeriría que hay otro
		// alcance posible.
		err := tx.QueryRow(ctx,
			"SELECT estado::text FROM negocio.reserva WHERE id = $1 FOR UPDATE",
			reservaID).Scan(&actual)
		if errors.Is(err, pgx.ErrNoRows) {
			return datos.ErrNoEncontrado
		}
		if err != nil {
			return err
		}

		desde := api.EstadoReserva(actual)
		motivo, permitida := transicionesManuales[desde][cambio.Estado]
		if !permitida {
			return ErrTransicionInvalida{Desde: desde, Hasta: cambio.Estado}
		}
		if cambio.Motivo != nil && *cambio.Motivo != "" {
			motivo = *cambio.Motivo
		}

		// expira_en se limpia al salir de pendiente. Ninguna de estas tres
		// transiciones sale de pendiente, así que no hace falta tocarlo, y
		// tocarlo de más sería borrar el rastro de cuándo iba a vencer.
		if _, err := tx.Exec(ctx,
			"UPDATE negocio.reserva SET estado = $2::negocio.estado_reserva WHERE id = $1",
			reservaID, string(cambio.Estado)); err != nil {
			return err
		}

		// El historial que el cliente puede ver (RF-28), en la misma
		// transacción que el cambio.
		if _, err := tx.Exec(ctx, `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo,
				 actor_tipo, actor_id, motivo)
			VALUES ($1, $2, $3::negocio.estado_reserva, $4::negocio.estado_reserva,
			        'administrador', $5, $6)`,
			tenant, reservaID, actual, string(cambio.Estado), alcance.Cuenta, motivo); err != nil {
			return err
		}

		// Y la traza de seguridad, que es otra cosa: aquella cuenta la vida de
		// esta reserva y se le enseña a su dueño; esta registra quién hizo qué
		// en todo el sistema (RF-36).
		if err := auditoria.Escribir(ctx, tx, tenant, actor, auditoria.Evento{
			Accion:      "estado_" + string(cambio.Estado),
			RecursoTipo: auditoria.RecursoReserva,
			RecursoID:   reservaID.String(),
			Resultado:   auditoria.Exito,
		}); err != nil {
			return err
		}

		reserva, err = reservas.Escanear(tx.QueryRow(ctx,
			`SELECT `+reservas.Columnas+` FROM negocio.reserva WHERE id = $1`, reservaID))
		if err != nil {
			return err
		}

		return emitir(ctx, tx, tenant, EventoReservaEstadoCambiado, reserva)
	})
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}
