package trabajadores

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
)

// El motor de notificaciones de RF-10, en su versión mínima.
//
// Consume los eventos que el relay puso en JetStream y manda el correo que
// corresponda, reutilizando internal/correo, que ya entrega contra Mailpit.
// Es lo que cierra el círculo: reservar y recibir una confirmación, con el
// evento atravesando outbox -> NATS -> aquí.
//
// LO QUE NO ES, y conviene tenerlo escrito para que nadie lo confunda con
// RF-10 entero:
//
//	Sin preferencias por canal (RF-21) ni configuración por tenant (RF-16).
//	Las dos viven en tablas que todavía no existen; hasta entonces se manda a
//	todo el mundo por correo.
//
//	Sin plantillas. El texto está aquí dentro. Cuando exista RF-16 el negocio
//	podrá cambiarlo, y entonces esto lee de una tabla.
//
//	Sin registro de lo enviado. notificacion_programada es de ER-03 y tampoco
//	existe, así que hoy no se puede responder "¿se le avisó?" mirando la base,
//	solo mirando el buzón.
//
// Lo que SÍ tiene, porque no es opcional: es idempotente. La entrega del outbox
// es al-menos-una-vez, así que este consumidor recibirá duplicados, y JetStream
// los descarta solo dentro de su ventana de deduplicación.

// Notificador consume eventos y manda correo.
type Notificador struct {
	emisor   correo.Emisor
	registro *slog.Logger
}

func NuevoNotificador(emisor correo.Emisor, registro *slog.Logger) *Notificador {
	return &Notificador{emisor: emisor, registro: registro}
}

// eventoReserva es lo que el núcleo puso en el payload.
type eventoReserva struct {
	ReservaID string `json:"reserva_id"`
	Estado    string `json:"estado"`
	Periodo   struct {
		Inicio time.Time `json:"inicio"`
		Fin    time.Time `json:"fin"`
	} `json:"periodo"`
	ContactoNombre string `json:"contacto_nombre"`
	ContactoEmail  string `json:"contacto_email"`
}

// Suscribir arranca el consumidor y bloquea hasta que se cancele el contexto.
func (n *Notificador) Suscribir(ctx context.Context, js jetstream.JetStream) error {
	// Consumidor DURABLE y con nombre. Sin nombre, cada reinicio del proceso
	// empezaría un consumidor nuevo desde el principio del flujo y reenviaría
	// semanas de correos. Con nombre, JetStream recuerda por dónde iba.
	consumidor, err := js.CreateOrUpdateConsumer(ctx, FlujoEventos, jetstream.ConsumerConfig{
		Durable:       "notificador",
		AckPolicy:     jetstream.AckExplicitPolicy,
		FilterSubject: AsuntoEventos,

		// Cinco intentos y a la cola de los que nadie pudo entregar. Reintentar
		// indefinidamente un correo que siempre falla —una dirección que no
		// existe— bloquearía la cola detrás de él.
		MaxDeliver: 5,
		BackOff:    []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute},
	})
	if err != nil {
		return fmt.Errorf("no se pudo crear el consumidor de notificaciones: %w", err)
	}

	suscripcion, err := consumidor.Consume(func(mensaje jetstream.Msg) {
		n.procesar(ctx, mensaje)
	})
	if err != nil {
		return fmt.Errorf("no se pudo consumir: %w", err)
	}
	defer suscripcion.Stop()

	n.registro.Info("notificador suscrito", slog.String("flujo", FlujoEventos))

	<-ctx.Done()
	return nil
}

func (n *Notificador) procesar(ctx context.Context, mensaje jetstream.Msg) {
	var evento eventoReserva
	if err := json.Unmarshal(mensaje.Data(), &evento); err != nil {
		// Un payload ilegible no mejora reintentándolo. Se descarta con
		// Term para que no vuelva: dejarlo reintentar cinco veces solo
		// retrasaría lo que hay detrás.
		n.registro.ErrorContext(ctx, "evento ilegible; se descarta",
			slog.String("error", err.Error()))
		_ = mensaje.Term()
		return
	}

	// Sin correo no hay nada que hacer: es una reserva de cuenta, y ese camino
	// llega con RF-12 y las preferencias de RF-21.
	if evento.ContactoEmail == "" {
		_ = mensaje.Ack()
		return
	}

	asunto, cuerpo, hay := n.mensajePara(mensaje.Subject(), evento)
	if !hay {
		// Un evento que este consumidor no traduce a correo no es un error: el
		// flujo lleva todo lo que pasa en el sistema y esto solo atiende una
		// parte.
		_ = mensaje.Ack()
		return
	}

	if err := n.emisor.Enviar(ctx, correo.Mensaje{
		Para:   evento.ContactoEmail,
		Asunto: asunto,
		Cuerpo: cuerpo,
	}); err != nil {
		// NAK y no Ack: que JetStream lo reparta otra vez con su espera. Un
		// relé caído se arregla solo, y perder el aviso porque el SMTP tardó
		// dos segundos de más sería exactamente lo que el outbox vino a evitar.
		n.registro.WarnContext(ctx, "no se pudo notificar; se reintentará",
			slog.String("reserva", evento.ReservaID),
			slog.String("error", err.Error()))
		_ = mensaje.Nak()
		return
	}

	_ = mensaje.Ack()
}

func (n *Notificador) mensajePara(asunto string, e eventoReserva) (string, string, bool) {
	cuando := e.Periodo.Inicio.Format("02/01/2006 15:04")

	switch asunto {
	case "reservas." + tipoCreada:
		return "Tu reserva está apartada", fmt.Sprintf(
			"Hola %s:\n\n"+
				"Te hemos apartado el horario del %s.\n\n"+
				"Todavía está PENDIENTE de pago: si no se completa a tiempo, el horario\n"+
				"vuelve a quedar libre para otra persona.\n",
			e.ContactoNombre, cuando), true

	case "reservas." + tipoCancelada:
		return "Tu reserva se canceló", fmt.Sprintf(
			"Hola %s:\n\n"+
				"Tu reserva del %s queda cancelada.\n\n"+
				"El horario vuelve a estar disponible.\n",
			e.ContactoNombre, cuando), true

	case "reservas." + tipoExpirada:
		return "El horario que tenías apartado venció", fmt.Sprintf(
			"Hola %s:\n\n"+
				"El horario del %s que tenías apartado ha vuelto a quedar libre porque\n"+
				"no se completó el pago a tiempo.\n\n"+
				"Si todavía lo quieres, puedes volver a reservarlo.\n",
			e.ContactoNombre, cuando), true

	default:
		return "", "", false
	}
}

// Los tipos se repiten aquí en vez de importarse de internal/nucleo: el
// consumidor no debe depender del productor. Hoy es el núcleo quien los emite;
// mañana serán también el webhook de pagos y el administrador, y ninguno de
// ellos debería tener que importar a los otros para nombrar un evento.
const (
	tipoCreada    = "reserva.creada"
	tipoCancelada = "reserva.cancelada"
	tipoExpirada  = "reserva.expirada"
)
