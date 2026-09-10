package nucleo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/reservas"
)

// Reprogramación de una reserva (RF-07).
//
// Vive en el núcleo por la misma razón que crear y cancelar, y en su caso con
// más motivo todavía: mover una reserva LIBERA un cupo y TOMA otro, así que
// atraviesa la invariante de RNF-10 en las dos direcciones a la vez.
//
// Y por eso es un UPDATE y no un cancelar-y-crear. Aquella secuencia tiene dos
// problemas que esta no tiene: entre las dos operaciones el cupo viejo queda
// libre —y otro cliente puede llevárselo mientras el primero se queda sin
// ninguno de los dos— y la reserva nueva pierde la historia, el precio
// congelado y la política de la vieja. Con un UPDATE dentro de una transacción,
// la restricción EXCLUDE arbitra el horario nuevo y el viejo se libera solo al
// confirmar, sin que exista un instante intermedio visible.

var (
	// ErrNoModificable: la reserva ya no está en un estado desde el que se
	// pueda mover. Es distinto de "fuera de plazo": aquí no hay nada que
	// reprogramar, no es que no se permita.
	ErrNoModificable = errors.New("la reserva ya no se puede modificar en su estado actual")

	// ErrRecursoNoEquivalente: el recurso nuevo no presta el mismo servicio.
	// Cambiar de servicio cambiaría el precio y la duración, y eso ya no es
	// reprogramar: es otra reserva.
	ErrRecursoNoEquivalente = errors.New(
		"el recurso nuevo no presta el mismo servicio que la reserva")
)

// ErrPlazoModificacionVencido lleva el detalle que la interfaz necesita.
//
// Es un tipo aparte de ErrPlazoVencido y no el mismo con otro número: los dos
// plazos salen de columnas distintas de la política y un negocio puede fijarlos
// distintos —modificar hasta 2 horas antes, cancelar hasta 24— así que un
// mensaje que dijera "cancelación" cuando lo que falló fue la modificación
// mandaría a la persona a mirar la regla equivocada.
type ErrPlazoModificacionVencido struct {
	HorasRequeridas int
	HorasRestantes  float64
}

func (e ErrPlazoModificacionVencido) Error() string {
	return fmt.Sprintf(
		"esta reserva se podía modificar hasta %d horas antes, y ya faltan menos de %.0f",
		e.HorasRequeridas, e.HorasRestantes)
}

func (e ErrPlazoModificacionVencido) Is(objetivo error) bool { return objetivo == ErrFueraDePlazo }

// estadosModificables son los únicos desde los que se puede mover una reserva.
//
// Los mismos que se pueden cancelar, y no por casualidad: son exactamente los
// que ocupan cupo. Una completada ya ocurrió y moverla sería reescribir lo que
// pasó; una cancelada o expirada no tiene cupo que mover.
var estadosModificables = map[api.EstadoReserva]bool{
	api.Pendiente:  true,
	api.Confirmada: true,
}

// Modificar mueve la reserva a otro horario, y opcionalmente a otro recurso.
func (s *Servicio) Modificar(
	ctx context.Context, tenant, reservaID uuid.UUID,
	alcance dominio.Alcance, actor auditoria.Actor,
	cambio api.ModificacionReserva,
) (api.Reserva, error) {
	if alcance.Vacio() {
		return api.Reserva{}, dominio.ErrSinAlcance
	}

	periodo := dominio.Periodo{Inicio: cambio.Periodo.Inicio, Fin: cambio.Periodo.Fin}
	if !periodo.Valido() {
		return api.Reserva{}, dominio.ErrPeriodoInvalido
	}
	if !periodo.Inicio.After(time.Now()) {
		return api.Reserva{}, dominio.ErrPeriodoEnElPasado
	}

	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var (
			estado            string
			inicio            time.Time
			servicioID        string
			recursoActual     string
			horasModificacion int
		)

		// FOR UPDATE sobre la reserva, igual que al cancelar: dos
		// modificaciones simultáneas de la misma fila tienen que serializarse.
		// El alcance va en el WHERE y no en un `if` posterior, así que una
		// reserva ajena devuelve cero filas igual que una inexistente.
		err := tx.QueryRow(ctx, `
			SELECT r.estado::text, lower(r.periodo), r.servicio_id::text, r.recurso_id::text,
			       p.rango_modificacion_horas
			FROM negocio.reserva r
			JOIN negocio.politica_version p
			  ON p.tenant_id = r.tenant_id AND p.id = r.politica_version_id
			WHERE r.id = $1
			  AND (
			        $2::boolean
			        OR ($3::uuid IS NOT NULL AND r.cuenta_id = $3::uuid)
			        OR ($4::text IS NOT NULL
			            AND r.cuenta_id IS NULL
			            AND lower(r.contacto_email) = $4::text)
			      )
			FOR UPDATE OF r`,
			reservaID, alcance.TenantCompleto, nulo(alcance.Cuenta), nulo(alcance.Destino),
		).Scan(&estado, &inicio, &servicioID, &recursoActual, &horasModificacion)
		if errors.Is(err, pgx.ErrNoRows) {
			return datos.ErrNoEncontrado
		}
		if err != nil {
			return err
		}

		if !estadosModificables[api.EstadoReserva(estado)] {
			return ErrNoModificable
		}

		// El plazo se mide contra el inicio ACTUAL de la cita, no contra el
		// nuevo: la regla acota cuánto puede tardar alguien en decidir sobre la
		// cita que tiene, y medirla contra la nueva permitiría escapar del
		// plazo eligiendo una fecha lejana.
		//
		// Al administrador no se le aplica (RF-32), por lo mismo que en la
		// cancelación: la política de RF-15 acota lo que puede hacer un
		// CLIENTE con la reserva que compró.
		restantes := time.Until(inicio).Hours()
		if !alcance.TenantCompleto && restantes < float64(horasModificacion) {
			return ErrPlazoModificacionVencido{
				HorasRequeridas: horasModificacion,
				HorasRestantes:  restantes,
			}
		}

		recursoNuevo := recursoActual
		if cambio.RecursoId != nil {
			recursoNuevo = cambio.RecursoId.String()
		}

		oferta, err := leerOferta(ctx, tx, uuid.MustParse(servicioID), uuid.MustParse(recursoNuevo))
		if err != nil {
			return err
		}
		if !oferta.presta || !oferta.recursoActivo {
			return ErrRecursoNoEquivalente
		}
		if !periodo.CoincideCon(oferta.duracionMin) {
			// La duración la fija el servicio y no cambia al reprogramar: el
			// precio se congeló sobre ella, así que una cita de otra duración
			// al mismo precio sería otra cosa vendida como la misma.
			return dominio.ErrDuracionNoCoincide
		}

		abierto, err := estaEnHorario(ctx, tx, uuid.MustParse(recursoNuevo), periodo)
		if err != nil {
			return err
		}
		if !abierto {
			return dominio.ErrFueraDeHorario
		}

		// Se reciclan las pendientes vencidas del destino, igual que al crear:
		// el predicado de la restricción EXCLUDE no puede excluirlas —now() no
		// es inmutable— así que una pendiente muerta seguiría bloqueando un
		// cupo que la disponibilidad ya muestra libre.
		if err := reciclarVencidas(ctx, tx, tenant, uuid.MustParse(recursoNuevo), periodo); err != nil {
			return err
		}

		// Y aquí decide el motor. No se comprueba disponibilidad y luego se
		// mueve: se mueve, y un 23P01 sobre la reserva es ErrHorarioOcupado.
		if _, err := tx.Exec(ctx, `
			UPDATE negocio.reserva
			SET recurso_id = $2,
			    periodo    = tstzrange($3::timestamptz, $4::timestamptz, '[)')
			WHERE id = $1`,
			reservaID, recursoNuevo, periodo.Inicio, periodo.Fin); err != nil {
			return err
		}

		reserva, err = reservas.Escanear(tx.QueryRow(ctx,
			`SELECT `+reservas.Columnas+` FROM negocio.reserva WHERE id = $1`, reservaID))
		if err != nil {
			return err
		}

		// La traza va a evento_auditoria y NO a transicion_estado, y la razón
		// es del modelo: transicion_estado exige que el estado cambie
		// (transicion_cambia_algo) y aquí no cambia —una reserva movida sigue
		// pendiente o confirmada—. RF-28 dibuja una flecha "reprogramada", pero
		// negocio.estado_reserva no tiene ese valor y no debería tenerlo: no es
		// un estado, es un cambio de horario. Quien pregunte "quién la movió"
		// tiene la respuesta en la auditoría de RF-36.
		if err := auditoria.Escribir(ctx, tx, tenant, actor, auditoria.Evento{
			Accion:      "modificar_reserva",
			RecursoTipo: auditoria.RecursoReserva,
			RecursoID:   reservaID.String(),
			Resultado:   auditoria.Exito,
		}); err != nil {
			return err
		}

		// El evento, en la misma transacción. RF-07 pide avisar de los cambios,
		// y ese aviso lo manda el notificador cuando el relay le entregue esto.
		return emitir(ctx, tx, tenant, EventoReservaModificada, reserva)
	})
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}
