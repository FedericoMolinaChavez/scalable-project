package nucleo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
)

// El outbox: cómo sale un evento del núcleo sin poder perderse ni sobrar.
//
// Se escribe DENTRO de la transacción que cambia la reserva, no después. Es
// literalmente lo que dice la nota del núcleo en ARQ-01, y la razón es que las
// dos alternativas están rotas:
//
//	Publicar tras el COMMIT   el proceso puede morir en medio. La reserva
//	                          existe y el evento no: nadie manda la
//	                          confirmación y nadie se entera de que faltó.
//
//	Publicar antes del COMMIT la transacción todavía puede revertirse. Queda
//	                          un evento sobre una reserva que nunca existió.
//
// Escribiéndolo en la misma transacción, el evento tiene exactamente el mismo
// destino que el cambio que lo causó. Un relay aparte lo lleva a NATS.

// Tipos de evento. Son cadenas y no un enum del esquema a propósito: el outbox
// transporta eventos de todo el sistema, y cada componente añadirá los suyos
// sin que eso obligue a una migración.
const (
	EventoReservaCreada    = "reserva.creada"
	EventoReservaCancelada = "reserva.cancelada"
	EventoReservaExpirada  = "reserva.expirada"
)

// eventoReserva es lo que viaja en el payload.
//
// Lleva lo justo para que un consumidor actúe sin volver a consultar: a quién
// avisar y de qué. NO lleva el precio ni los datos completos de la reserva,
// porque un evento es un aviso de que algo pasó, no una copia del estado: quien
// necesite el resto lo lee de la base, que es donde sigue siendo verdad.
type eventoReserva struct {
	ReservaID  uuid.UUID `json:"reserva_id"`
	ServicioID uuid.UUID `json:"servicio_id"`
	RecursoID  uuid.UUID `json:"recurso_id"`
	Estado     string    `json:"estado"`
	Periodo    struct {
		Inicio string `json:"inicio"`
		Fin    string `json:"fin"`
	} `json:"periodo"`

	// Contacto para poder notificar sin releer la reserva, que es el caso de
	// uso del 90% de los consumidores (RF-10).
	ContactoNombre string `json:"contacto_nombre,omitempty"`
	ContactoEmail  string `json:"contacto_email,omitempty"`
}

// emitir escribe un evento en el outbox, dentro de la transacción en curso.
func emitir(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, tipo string, reserva api.Reserva) error {
	evento := eventoReserva{
		ReservaID:  reserva.Id,
		ServicioID: reserva.ServicioId,
		RecursoID:  reserva.RecursoId,
		Estado:     string(reserva.Estado),
	}
	evento.Periodo.Inicio = reserva.Periodo.Inicio.UTC().Format(marcaTiempo)
	evento.Periodo.Fin = reserva.Periodo.Fin.UTC().Format(marcaTiempo)

	if reserva.Contacto != nil {
		evento.ContactoNombre = reserva.Contacto.Nombre
		evento.ContactoEmail = string(reserva.Contacto.Email)
	}

	payload, err := json.Marshal(evento)
	if err != nil {
		return fmt.Errorf("no se pudo serializar el evento %s: %w", tipo, err)
	}

	// El payload va como texto con conversión explícita a jsonb. Un []byte lo
	// manda pgx como bytea, y Postgres no convierte bytea a jsonb: el error que
	// sale es "invalid input syntax for type json", que apunta al contenido
	// cuando el problema es el tipo con el que viajó.
	_, err = tx.Exec(ctx, `
		INSERT INTO negocio.outbox_evento (tenant_id, tipo, payload)
		VALUES ($1, $2, $3::jsonb)`,
		tenant, tipo, string(payload))
	if err != nil {
		return fmt.Errorf("no se pudo escribir el evento %s en el outbox: %w", tipo, err)
	}

	return nil
}

// marcaTiempo es RFC 3339 con nanosegundos, el mismo formato que usa el
// contrato. Un evento y una respuesta de la API que hablen de la misma reserva
// tienen que decir la misma hora escrita igual.
const marcaTiempo = "2006-01-02T15:04:05.999999999Z07:00"
