package pagos

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// El webhook de RF-33: el único sitio donde una reserva pasa a confirmada.
//
// Vive fuera del router generado desde el contrato, y no es una excepción
// caprichosa. La firma de Stripe se calcula sobre los BYTES EXACTOS del cuerpo;
// un manejador generado lo decodificaría a una estructura antes de que nadie la
// verificara, y volver a serializarlo produce bytes distintos —orden de claves,
// espacios, precisión numérica— con los que la firma ya no cuadra. Verificar
// primero exige leer el cuerpo crudo, así que el manejador es de net/http a
// secas.
//
// Tampoco es una ruta del contrato en otro sentido: no la llama ningún cliente
// de esta API. La llama Stripe.

// LimiteCuerpo acota lo que se lee del cuerpo antes de verificar la firma.
//
// Hace falta porque este manejador es público y sin autenticación previa: la
// firma se comprueba DESPUÉS de leer, así que sin límite cualquiera puede
// mandar un cuerpo de gigabytes y hacer que el proceso lo cargue entero en
// memoria antes de rechazarlo. Un evento de Stripe cabe holgadamente en 64 KiB.
const LimiteCuerpo = 64 << 10

// Manejador construye el http.Handler del webhook.
func (s *Servicio) Manejador() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cuerpo, err := io.ReadAll(http.MaxBytesReader(w, r.Body, LimiteCuerpo))
		if err != nil {
			http.Error(w, "cuerpo ilegible", http.StatusBadRequest)
			return
		}

		evento, err := s.pasarela.VerificarEvento(cuerpo, r.Header.Get("Stripe-Signature"))
		if err != nil {
			// Un evento con firma mala se descarta y se registra, y NO se
			// guarda en la bitácora: la tabla es el registro de lo que Stripe
			// mandó, y esto no viene de Stripe. Guardarlo dejaría que cualquiera
			// escribiera en ella desde fuera.
			s.registro.WarnContext(r.Context(), "webhook con firma inválida; se descarta",
				slog.String("error", err.Error()),
				slog.String("remoto", r.RemoteAddr))
			http.Error(w, "firma inválida", http.StatusBadRequest)
			return
		}

		// A partir de aquí el evento es auténtico, y la respuesta a Stripe es
		// 200 salvo que algo nuestro falle. Responder un error por un evento
		// que no sabemos procesar haría que Stripe lo reintentara durante días
		// sin que el resultado cambiara nunca.
		if err := s.Procesar(r.Context(), evento); err != nil {
			// Un 500 SÍ pide el reintento, y aquí sí queremos uno: el evento es
			// válido y el fallo es nuestro —la base no respondió—, así que la
			// siguiente entrega tiene posibilidades de salir bien.
			s.registro.ErrorContext(r.Context(), "no se pudo procesar el webhook",
				slog.String("evento", evento.ID),
				slog.String("tipo", evento.Tipo),
				slog.String("error", err.Error()))
			http.Error(w, "no se pudo procesar", http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}
}

// Procesar aplica el efecto de negocio de un evento ya verificado.
//
// Es idempotente por construcción, y tiene que serlo: Stripe entrega
// at-least-once y reintenta durante días ante cualquier respuesta que no sea
// 2xx. La clave es plataforma.evento_webhook.evento_id; el segundo intento se
// detecta al insertar y no al consultar, porque dos entregas simultáneas del
// mismo evento pasarían las dos por un `SELECT` previo.
func (s *Servicio) Procesar(ctx context.Context, evento Evento) error {
	nuevo, err := s.registrarEvento(ctx, evento)
	if err != nil {
		return err
	}
	if !nuevo {
		s.registro.DebugContext(ctx, "evento ya procesado; se ignora",
			slog.String("evento", evento.ID))
		return nil
	}

	errProceso := s.aplicar(ctx, evento)

	// El resultado se marca pase lo que pase. Un evento que falló queda con su
	// error en la bitácora en vez de desaparecer: es la diferencia entre poder
	// responder "el cobro llegó y no se pudo aplicar" y no saber que llegó.
	if err := s.marcarProcesado(ctx, evento.ID, errProceso); err != nil {
		return err
	}

	return errProceso
}

// registrarEvento escribe la llegada y dice si es la primera.
func (s *Servicio) registrarEvento(ctx context.Context, evento Evento) (bool, error) {
	nuevo := false

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		// ON CONFLICT DO NOTHING y RETURNING: la fila solo vuelve si se
		// insertó. Es la forma de preguntar "¿es nuevo?" sin una carrera entre
		// consultar y escribir.
		var id string
		err := tx.QueryRow(ctx, `
			INSERT INTO plataforma.evento_webhook (proveedor, evento_id, tipo, payload)
			VALUES ('stripe', $1, $2, $3::jsonb)
			ON CONFLICT (proveedor, evento_id) DO NOTHING
			RETURNING id::text`,
			evento.ID, evento.Tipo, string(evento.Crudo)).Scan(&id)

		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		nuevo = true
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("no se pudo registrar el evento %s: %w", evento.ID, err)
	}

	return nuevo, nil
}

func (s *Servicio) marcarProcesado(ctx context.Context, eventoID string, fallo error) error {
	var mensaje *string
	if fallo != nil {
		texto := fallo.Error()
		mensaje = &texto
	}

	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE plataforma.evento_webhook
			SET procesado_en = now(), error = $2
			WHERE evento_id = $1 AND proveedor = 'stripe'`,
			eventoID, mensaje)
		return err
	})
}

// aplicar reparte el evento entre los tipos que este componente atiende.
func (s *Servicio) aplicar(ctx context.Context, evento Evento) error {
	switch evento.Tipo {
	case "payment_intent.succeeded":
		return s.cobroConfirmado(ctx, evento)

	case "payment_intent.payment_failed":
		// NO marca el pago como fallido, y eso es deliberado. En Stripe, una
		// tarjeta rechazada deja el PaymentIntent listo para otro intento, no
		// cerrado; darlo por muerto aquí cerraría el bucle de reintento que
		// RF-01 describe ("¿Reintenta el pago?") y obligaría a crear una
		// intención nueva por cada tarjeta que falle. Lo que sí se guarda es el
		// motivo, que es lo que la interfaz enseña.
		return s.anotarRechazo(ctx, evento)

	case "payment_intent.canceled":
		return s.cobroCancelado(ctx, evento)

	default:
		// Ni error ni efecto. El evento ya quedó en la bitácora, que es lo que
		// permite responder después "¿qué llegó de Stripe?" incluyendo lo que
		// todavía no se procesa.
		return nil
	}
}

// cobroConfirmado es el camino feliz de RF-33 y el único que confirma reservas
// por el lado del webhook. El efecto en sí vive en AplicarCobro, compartido con
// el conciliador: si Stripe no avisa, la reserva se confirma igual y de la
// misma manera.
func (s *Servicio) cobroConfirmado(ctx context.Context, evento Evento) error {
	tenant, reserva, err := identificadores(evento)
	if err != nil {
		// Un cobro que no dice de quién es. Solo puede venir de un
		// PaymentIntent creado fuera de este sistema; se registra el problema y
		// se devuelve el dinero, porque el importe está cobrado de verdad.
		return s.devolverHuerfano(ctx, evento, "el evento no trae tenant ni reserva en su metadata")
	}

	var desenlace Desenlace

	err = s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var err error
		desenlace, err = AplicarCobro(ctx, tx, tenant, reserva, evento.Intencion.ID)
		if err != nil {
			return err
		}

		// El reembolso se anota en la MISMA transacción que descubrió que hacía
		// falta. Fuera de ella, un fallo entre ambas dejaría un cobro que nadie
		// sabe que hay que devolver, y eso es dinero de una persona concreta.
		if desenlace == CobroSinCupo {
			return AnotarReembolso(ctx, tx, evento.Intencion.ID, "sin_cupo")
		}
		return nil
	})
	if err != nil {
		return err
	}

	if desenlace == CobroSinCupo {
		s.registro.WarnContext(ctx, "cobro sobre una reserva que ya no se puede honrar; se devolverá",
			slog.String("intencion", evento.Intencion.ID),
			slog.String("tenant", tenant.String()))
	}

	return nil
}

// anotarRechazo guarda por qué se rechazó el último intento, sin cerrar nada.
func (s *Servicio) anotarRechazo(ctx context.Context, evento Evento) error {
	tenant, _, err := identificadores(evento)
	if err != nil {
		return nil // un rechazo sin dueño no tiene nada que anotar
	}

	motivo := evento.Intencion.MotivoFallo
	if motivo == "" {
		motivo = "el proveedor rechazó el cobro sin dar un motivo"
	}

	return s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return MarcarFalloDePago(ctx, tx, evento.Intencion.ID, motivo)
	})
}

// cobroCancelado cierra el intento.
func (s *Servicio) cobroCancelado(ctx context.Context, evento Evento) error {
	tenant, _, err := identificadores(evento)
	if err != nil {
		return nil
	}

	return s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return CerrarPago(ctx, tx, evento.Intencion.ID, "el intento de pago se canceló")
	})
}

// devolverHuerfano atiende el cobro que no se puede atribuir a nadie.
//
// Sin fila de pago no hay dónde escribir un reembolso, así que este es el único
// sitio del sistema que llama a Stripe desde el manejador del webhook. Se
// acepta porque la alternativa es peor: quedarse con dinero de alguien a quien
// no se le puede dar nada a cambio.
func (s *Servicio) devolverHuerfano(ctx context.Context, evento Evento, razon string) error {
	s.registro.ErrorContext(ctx, "cobro huérfano; se devuelve",
		slog.String("intencion", evento.Intencion.ID),
		slog.String("razon", razon))

	// Contexto propio y acotado: el de la petición lo cancela Stripe si cierra
	// la conexión, y esta devolución tiene que salir igual.
	ctxDevolucion, cancelar := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancelar()

	// Monto vacío: sin fila de pago no se conoce el importe, y Stripe entiende
	// un reembolso sin monto como "todo lo cobrado", que es exactamente lo que
	// corresponde a un cobro que no debía haberse hecho.
	if _, err := s.pasarela.Reembolsar(ctxDevolucion, evento.Intencion.ID, Monto{}, "sin_cupo"); err != nil {
		return fmt.Errorf("cobro huérfano %s (%s) y no se pudo devolver: %w",
			evento.Intencion.ID, razon, err)
	}

	return fmt.Errorf("cobro huérfano %s: %s; devuelto", evento.Intencion.ID, razon)
}

// identificadores saca el tenant y la reserva de la metadata del evento.
func identificadores(evento Evento) (uuid.UUID, uuid.UUID, error) {
	tenant, err := uuid.Parse(evento.Tenant)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("tenant ilegible en el evento %s: %w", evento.ID, err)
	}

	reserva, err := uuid.Parse(evento.Reserva)
	if err != nil {
		return uuid.Nil, uuid.Nil, fmt.Errorf("reserva ilegible en el evento %s: %w", evento.ID, err)
	}

	return tenant, reserva, nil
}

// Comprobar es la verificación de la sonda: que la base responde.
//
// Stripe NO entra en /listo, y es el mismo criterio que se aplicó al SMTP en
// identidad y al caché en consulta: sin Stripe este servicio no puede abrir
// cobros nuevos, pero sí puede recibir y aplicar webhooks de cobros ya hechos,
// que es la mitad que de verdad no puede esperar. Sacar el pod del balanceador
// apagaría también esa mitad.
func (s *Servicio) Comprobar(ctx context.Context) error {
	return s.bd.Comprobar(ctx)
}
