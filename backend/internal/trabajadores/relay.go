package trabajadores

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// El relay del outbox: lleva a NATS lo que el núcleo escribió en PostgreSQL.
//
// Es la mitad que cierra el patrón. El núcleo escribe el evento dentro de su
// transacción y no publica nada; esto lo publica después, con reintentos, y
// marca la fila. Si muere entre publicar y marcar, la siguiente pasada
// republica: por eso la entrega es AL MENOS UNA VEZ y la deduplicación la hace
// JetStream con Nats-Msg-Id (ER-03).
//
// Ese identificador es el `id` del evento, que ya es único por construcción. No
// hace falta inventar nada: la fila que se republica lleva el mismo id, y
// JetStream descarta el duplicado dentro de su ventana.

// FlujoEventos es el stream de JetStream donde caen todos los eventos.
const (
	FlujoEventos  = "RESERVAS"
	AsuntoEventos = "reservas.>"
)

// Publicador abstrae JetStream para poder probar el relay sin NATS.
type Publicador interface {
	Publicar(ctx context.Context, asunto, idMensaje string, payload []byte) error
}

// Relay construye el bucle.
func Relay(bd *datos.BD, pub Publicador, intervalo time.Duration, registro *slog.Logger) Bucle {
	return Bucle{
		Nombre:    "relay",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return drenar(ctx, tx, pub, registro)
			})
		},
	}
}

func drenar(ctx context.Context, tx pgx.Tx, pub Publicador, registro *slog.Logger) (int, error) {
	filas, err := tx.Query(ctx, `
		SELECT id::text, tipo, payload::text
		FROM negocio.outbox_evento
		WHERE publicado_en IS NULL
		ORDER BY creado_en
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, LoteMaximo)
	if err != nil {
		return 0, err
	}

	type pendiente struct{ id, tipo, payload string }
	var lote []pendiente

	for filas.Next() {
		var p pendiente
		if err := filas.Scan(&p.id, &p.tipo, &p.payload); err != nil {
			filas.Close()
			return 0, err
		}
		lote = append(lote, p)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	if len(lote) == 0 {
		return 0, nil
	}

	// Se publica ANTES de marcar, y el orden no es negociable en este sentido.
	//
	// Marcar primero y publicar después perdería el evento si la publicación
	// falla: la fila diría "publicado" y NATS no tendría nada. Publicando
	// primero, el peor caso es publicar dos veces, que es justo lo que
	// Nats-Msg-Id resuelve. Se prefiere el duplicado sobre la pérdida porque un
	// duplicado lo absorbe un consumidor idempotente y una pérdida no la
	// absorbe nadie.
	publicados := make([]string, 0, len(lote))
	for _, p := range lote {
		asunto := "reservas." + p.tipo

		if err := pub.Publicar(ctx, asunto, p.id, []byte(p.payload)); err != nil {
			// Se corta el lote aquí y se conserva lo ya publicado. Seguir con
			// el resto rompería el orden, y el orden dentro de un mismo
			// agregado importa: "cancelada" antes que "creada" no significa
			// nada.
			registro.WarnContext(ctx, "no se pudo publicar; se reintenta en la siguiente pasada",
				slog.String("evento", p.id), slog.String("error", err.Error()))
			break
		}
		publicados = append(publicados, p.id)
	}

	if len(publicados) == 0 {
		return 0, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE negocio.outbox_evento
		SET publicado_en = now()
		WHERE id = ANY($1::uuid[])`, publicados); err != nil {
		return 0, err
	}

	return len(publicados), nil
}

// --------------------------------------------------------------- JetStream --

// JetStream publica de verdad.
type JetStream struct {
	conexion *nats.Conn
	flujo    jetstream.JetStream
}

// AbrirJetStream conecta y se asegura de que el flujo existe.
func AbrirJetStream(ctx context.Context, url string) (*JetStream, error) {
	conexion, err := nats.Connect(url,
		// Reconexión indefinida: un trabajador vive semanas y NATS se
		// reiniciará alguna vez. Rendirse tras N intentos convertiría un
		// reinicio de treinta segundos en un relay parado hasta que alguien lo
		// note.
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con NATS en %s: %w", url, err)
	}

	js, err := jetstream.New(conexion)
	if err != nil {
		conexion.Close()
		return nil, fmt.Errorf("no se pudo abrir JetStream: %w", err)
	}

	// El flujo se crea si no está. Es idempotente, así que arrancar varias
	// réplicas a la vez no es un problema.
	if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:     FlujoEventos,
		Subjects: []string{AsuntoEventos},

		// La ventana de deduplicación: dentro de ella, dos mensajes con el
		// mismo Nats-Msg-Id son uno. Tiene que ser holgadamente mayor que el
		// tiempo que puede pasar entre publicar y marcar la fila, que es lo
		// que dura una transacción: con dos minutos sobra por mucho.
		Duplicates: 2 * time.Minute,

		// Retención por límite y no por reconocimiento: los eventos son un
		// registro de lo que pasó, no una cola de trabajo que se vacía. Varios
		// consumidores distintos leen los mismos eventos.
		Retention: jetstream.LimitsPolicy,
		MaxAge:    7 * 24 * time.Hour,
		Storage:   jetstream.FileStorage,
	}); err != nil {
		conexion.Close()
		return nil, fmt.Errorf("no se pudo crear el flujo %s: %w", FlujoEventos, err)
	}

	return &JetStream{conexion: conexion, flujo: js}, nil
}

func (j *JetStream) Cerrar() {
	// Drain y no Close: espera a que salgan los mensajes en vuelo en vez de
	// cortarlos, que en un apagado ordenado es la diferencia entre perder un
	// evento y no perderlo.
	_ = j.conexion.Drain()
}

func (j *JetStream) Publicar(ctx context.Context, asunto, idMensaje string, payload []byte) error {
	_, err := j.flujo.Publish(ctx, asunto, payload, jetstream.WithMsgID(idMensaje))
	return err
}

// Conexion expone la conexión para el consumidor y las sondas.
func (j *JetStream) Conexion() jetstream.JetStream { return j.flujo }

// Comprobar dice si NATS responde.
func (j *JetStream) Comprobar(context.Context) error {
	if !j.conexion.IsConnected() {
		return fmt.Errorf("sin conexión con NATS")
	}
	return nil
}
