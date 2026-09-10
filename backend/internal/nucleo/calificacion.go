package nucleo

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
)

// La calificación de RF-20.
//
// Vive en el núcleo aunque no toque negocio.reserva y no compita por ningún
// cupo, y la razón es la regla: "solo sobre una reserva completada" exige leer
// el estado de la reserva y decidir, y esa decisión tiene que ver la misma fila
// que la escritura. Repartirla —comprobar el estado en la ruta de lectura y
// escribir aquí— dejaría una ventana en la que la reserva cambia de estado
// entre las dos.
//
// Las dos reglas del requisito están en sitios distintos a propósito, y así lo
// dice el esquema: la unicidad la sostiene el motor con
// `calificacion_una_por_reserva` porque es la que se ataca desde fuera; "solo
// completada" no cabe en un CHECK —exige mirar otra tabla— y vive aquí.

var (
	// ErrNoCalificable: la reserva no está completada. Calificar algo que
	// todavía no ocurrió no es una opinión, es una expectativa.
	ErrNoCalificable = errors.New("solo se puede calificar una reserva completada")

	// ErrYaCalificada: una por reserva (RF-20).
	ErrYaCalificada = errors.New("esa reserva ya tiene una calificación")
)

// Calificar registra la calificación de una reserva completada (RF-20).
func (s *Servicio) Calificar(
	ctx context.Context, tenant, reservaID uuid.UUID,
	alcance dominio.Alcance, nueva api.NuevaCalificacion,
) (api.Calificacion, error) {
	if alcance.Vacio() {
		return api.Calificacion{}, dominio.ErrSinAlcance
	}

	// El rango lo comprueban el contrato y el CHECK del esquema; aquí se
	// comprueba igualmente porque un puntaje fuera de rango que llegara por
	// otra ruta abortaría la transacción con un error del motor, y este dice
	// qué hacer.
	if nueva.Puntaje < 1 || nueva.Puntaje > 5 {
		return api.Calificacion{}, dominio.ErrPuntajeInvalido
	}

	var calificacion api.Calificacion

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var estado string

		// El alcance va en el WHERE: una reserva ajena devuelve cero filas
		// igual que una inexistente, y las dos salen como no encontrada.
		err := tx.QueryRow(ctx, `
			SELECT estado::text
			FROM negocio.reserva
			WHERE id = $1
			  AND (
			        $2::boolean
			        OR ($3::uuid IS NOT NULL AND cuenta_id = $3::uuid)
			        OR ($4::text IS NOT NULL
			            AND cuenta_id IS NULL
			            AND lower(contacto_email) = $4::text)
			      )`,
			reservaID, alcance.TenantCompleto, nulo(alcance.Cuenta), nulo(alcance.Destino),
		).Scan(&estado)
		if errors.Is(err, pgx.ErrNoRows) {
			return datos.ErrNoEncontrado
		}
		if err != nil {
			return err
		}

		if api.EstadoReserva(estado) != api.Completada {
			return ErrNoCalificable
		}

		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO negocio.calificacion (tenant_id, reserva_id, puntaje, comentario)
			VALUES ($1, $2, $3, $4)
			RETURNING id::text, puntaje, comentario, creada_en`,
			tenant, reservaID, nueva.Puntaje, nueva.Comentario,
		).Scan(&id, &calificacion.Puntaje, &calificacion.Comentario, &calificacion.CreadaEn)
		if err != nil {
			return err
		}

		if calificacion.Id, err = uuid.Parse(id); err != nil {
			return err
		}
		calificacion.ReservaId = reservaID
		return nil
	})

	// La unicidad la arbitra el índice, no una consulta previa: comprobar antes
	// e insertar después es una carrera con ventana, y dos envíos simultáneos
	// del mismo formulario la atraviesan los dos.
	if errors.Is(err, datos.ErrDuplicado) {
		return api.Calificacion{}, ErrYaCalificada
	}
	if err != nil {
		return api.Calificacion{}, err
	}

	return calificacion, nil
}
