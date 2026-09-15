package pagos_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Contra la base real y con Stripe sustituido, y esa asimetría es deliberada.
//
// PostgreSQL no se sustituye porque lo que se prueba vive dentro: el índice
// parcial que impide dos intentos vivos, el ON CONFLICT que hace idempotente el
// registro del evento, la transición de RF-28 y el outbox escritos en la misma
// transacción que la confirmación. Un doble los haría pasar sin comprobar nada.
//
// Stripe sí se sustituye, porque lo contrario es imposible: no hay forma de que
// una prueba automatizada haga que una tarjeta real falle a voluntad. Lo que la
// pasarela falsa permite es lo que de verdad hace falta comprobar —qué pasa
// cuando el webhook NO llega, cuando llega dos veces, o cuando llega por una
// reserva que ya expiró— y esos son justo los caminos que en producción nadie
// ejercita hasta que ocurren.

// pasarelaFalsa devuelve lo que la prueba le diga.
type pasarelaFalsa struct {
	estado      pagos.Estado
	motivoFallo string

	// Registro de lo que se le pidió. Las pruebas afirman sobre esto: lo que
	// importa de un reembolso no es solo que la fila quede marcada, es que se
	// haya llamado al proveedor exactamente una vez.
	reembolsos  []string
	cancelados  []string
	consultados int
}

func (p *pasarelaFalsa) CrearIntencion(context.Context, pagos.Solicitud) (pagos.Intencion, error) {
	return pagos.Intencion{
		ID:           "pi_" + uuid.NewString(),
		ClientSecret: "cs_de_prueba",
		Estado:       p.estado,
	}, nil
}

func (p *pasarelaFalsa) ConsultarIntencion(_ context.Context, id string) (pagos.Intencion, error) {
	p.consultados++
	return pagos.Intencion{
		ID:           id,
		ClientSecret: "cs_de_prueba",
		Estado:       p.estado,
		MotivoFallo:  p.motivoFallo,
	}, nil
}

func (p *pasarelaFalsa) CancelarIntencion(_ context.Context, id string) error {
	p.cancelados = append(p.cancelados, id)
	return nil
}

func (p *pasarelaFalsa) Reembolsar(
	_ context.Context, id string, _ pagos.Monto, _ string,
) (pagos.Reembolso, error) {
	p.reembolsos = append(p.reembolsos, id)
	return pagos.Reembolso{ID: "re_" + uuid.NewString(), Estado: pagos.ReembolsoConfirmado}, nil
}

func (p *pasarelaFalsa) ClavePublicable() string { return "pk_de_prueba" }

func (p *pasarelaFalsa) VerificarEvento([]byte, string) (pagos.Evento, error) {
	// Las pruebas construyen el Evento a mano y llaman a Procesar directamente:
	// verificar la firma es responsabilidad de la biblioteca de Stripe y
	// probarla aquí sería probar su código, no el nuestro.
	panic("las pruebas no pasan por la verificación de firma")
}

// ------------------------------------------------------------------ apoyo --

func servicioDePrueba(t *testing.T, bd *datos.BD) (*pagos.Servicio, *pasarelaFalsa) {
	t.Helper()

	falsa := &pasarelaFalsa{estado: pagos.Iniciado}
	registro := slog.New(slog.NewTextHandler(io.Discard, nil))

	return pagos.Nuevo(bd, falsa, nil, registro), falsa
}

// reservaPendiente crea una reserva de verdad, por el núcleo, sobre la franja
// que se le dé. No se inserta a mano: lo que la hace útil como punto de partida
// es que pasó por las mismas comprobaciones que en producción.
func reservaPendiente(t *testing.T, bd *datos.BD, inicio time.Time) api.Reserva {
	t.Helper()

	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso,
		inicio, inicio.Add(pruebas.DuracionServicio*time.Minute))

	servicio := nucleo.Nuevo(bd, 15*time.Minute)

	reserva, err := servicio.Crear(context.Background(), nucleo.Peticion{
		Tenant:            uuid.MustParse(pruebas.Tenant),
		ClaveIdempotencia: uuid.NewString(),
		Nueva: api.NuevaReserva{
			ServicioId: uuid.MustParse(pruebas.Servicio),
			RecursoId:  uuid.MustParse(pruebas.Recurso),
			Periodo: api.Periodo{
				Inicio: inicio,
				Fin:    inicio.Add(pruebas.DuracionServicio * time.Minute),
			},
			Contacto: api.Contacto{Nombre: "Prueba de pagos", Email: "pagos@ejemplo.test"},
		},
	})
	if err != nil {
		t.Fatalf("no se pudo crear la reserva de partida: %v", err)
	}

	return reserva
}

// eventoDeCobro arma el evento tal como saldría de VerificarEvento.
func eventoDeCobro(tipo, intencion string, reserva uuid.UUID) pagos.Evento {
	return pagos.Evento{
		ID:        "evt_" + uuid.NewString(),
		Tipo:      tipo,
		Tenant:    pruebas.Tenant,
		Reserva:   reserva.String(),
		Intencion: pagos.Intencion{ID: intencion, Estado: pagos.Confirmado},
		Crudo:     []byte(`{"prueba":true}`),
	}
}

func estadoDe(t *testing.T, bd *datos.BD, reserva uuid.UUID) (string, string) {
	t.Helper()

	var reservaEstado string
	var pagoEstado *string

	err := bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT r.estado::text,
			       (SELECT p.estado::text FROM negocio.pago p
			         WHERE p.reserva_id = r.id ORDER BY p.creado_en DESC LIMIT 1)
			FROM negocio.reserva r WHERE r.id = $1`, reserva).
			Scan(&reservaEstado, &pagoEstado)
	})
	if err != nil {
		t.Fatalf("no se pudo leer el estado: %v", err)
	}

	if pagoEstado == nil {
		return reservaEstado, ""
	}
	return reservaEstado, *pagoEstado
}

// ---------------------------------------------------------------- pruebas --

// El camino feliz de RF-33, entero: se abre el cobro, llega el webhook, y la
// reserva pasa de pendiente a confirmada con su transición y su evento.
func TestElWebhookConfirmaLaReservaPendiente(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, _ := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(9))

	intento, err := servicio.Intencion(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id)
	if err != nil {
		t.Fatalf("no se pudo abrir la intención: %v", err)
	}
	if intento.ClientSecret == "" {
		t.Fatal("la intención no trae client_secret: el navegador no podría pagar")
	}

	intencionID := intencionDe(t, bd, reserva.Id)

	if err := servicio.Procesar(ctx,
		eventoDeCobro("payment_intent.succeeded", intencionID, reserva.Id)); err != nil {
		t.Fatalf("el webhook falló: %v", err)
	}

	estadoReserva, estadoPago := estadoDe(t, bd, reserva.Id)
	if estadoReserva != "confirmada" {
		t.Errorf("la reserva quedó %q y debía quedar confirmada", estadoReserva)
	}
	if estadoPago != "confirmado" {
		t.Errorf("el pago quedó %q y debía quedar confirmado", estadoPago)
	}

	// La transición de RF-28 y el evento del outbox van en la MISMA transacción
	// que la confirmación. Sin ellos, la reserva estaría confirmada y nadie
	// recibiría el aviso, y no habría forma de saber que faltó.
	if !hayTransicion(t, bd, reserva.Id, "pendiente", "confirmada") {
		t.Error("no se escribió la transición pendiente → confirmada (RF-28)")
	}
	if !hayEvento(t, bd, reserva.Id, "reserva.confirmada") {
		t.Error("no se escribió el evento reserva.confirmada en el outbox")
	}
}

// Stripe entrega at-least-once y reintenta durante días ante cualquier
// respuesta que no sea 2xx, así que recibir el mismo evento dos veces es lo
// normal. La segunda no puede escribir una segunda transición.
func TestElMismoEventoDosVecesNoDuplicaNada(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, _ := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(10))
	if _, err := servicio.Intencion(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id); err != nil {
		t.Fatal(err)
	}

	evento := eventoDeCobro("payment_intent.succeeded", intencionDe(t, bd, reserva.Id), reserva.Id)

	for i := range 3 {
		if err := servicio.Procesar(ctx, evento); err != nil {
			t.Fatalf("entrega %d: %v", i+1, err)
		}
	}

	if n := transiciones(t, bd, reserva.Id, "confirmada"); n != 1 {
		t.Errorf("se escribieron %d transiciones a confirmada y debía ser 1", n)
	}
	if n := eventos(t, bd, reserva.Id, "reserva.confirmada"); n != 1 {
		t.Errorf("se escribieron %d eventos de confirmación y debía ser 1", n)
	}
}

// Pedir la intención dos veces sobre la misma reserva devuelve la MISMA, no una
// nueva. Sin esto, dos pestañas abiertas producirían dos PaymentIntent y se
// podrían pagar los dos: la restricción EXCLUDE protege el cupo, y nada
// protegería el dinero.
func TestPedirLaIntencionDosVecesNoAbreDosCobros(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, falsa := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(11))
	tenant := uuid.MustParse(pruebas.Tenant)

	if _, err := servicio.Intencion(ctx, tenant, reserva.Id); err != nil {
		t.Fatal(err)
	}
	primera := intencionDe(t, bd, reserva.Id)

	if _, err := servicio.Intencion(ctx, tenant, reserva.Id); err != nil {
		t.Fatal(err)
	}
	segunda := intencionDe(t, bd, reserva.Id)

	if primera != segunda {
		t.Errorf("se abrieron dos cobros para la misma reserva: %s y %s", primera, segunda)
	}
	if falsa.consultados == 0 {
		t.Error("la segunda llamada no reutilizó la intención existente: no la consultó")
	}
	if n := pagosDe(t, bd, reserva.Id); n != 1 {
		t.Errorf("hay %d filas de pago para una reserva y debía haber 1", n)
	}
}

// La rama incómoda de RF-33: el cobro llegó por una reserva que ya no se puede
// honrar. No se ignora —es dinero de una persona— y no se confirma —el cupo ya
// no está—: se anota el reembolso.
func TestUnCobroSobreUnaReservaExpiradaSeAnotaParaDevolver(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, _ := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(12))
	if _, err := servicio.Intencion(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id); err != nil {
		t.Fatal(err)
	}
	intencionID := intencionDe(t, bd, reserva.Id)

	// El bloqueo vence y el expirador se lo lleva, que es exactamente lo que
	// hace RF-27 mientras el pago está en vuelo.
	expirar(t, bd, reserva.Id)

	if err := servicio.Procesar(ctx,
		eventoDeCobro("payment_intent.succeeded", intencionID, reserva.Id)); err != nil {
		t.Fatalf("el webhook falló: %v", err)
	}

	estadoReserva, estadoPago := estadoDe(t, bd, reserva.Id)
	if estadoReserva != "expirada" {
		t.Errorf("la reserva quedó %q: un cobro tardío no puede resucitar un cupo liberado",
			estadoReserva)
	}
	// El pago SÍ se marca confirmado: el dinero se movió de verdad, y negarlo
	// en la base haría imposible cuadrarla con el extracto de Stripe.
	if estadoPago != "confirmado" {
		t.Errorf("el pago quedó %q y debía quedar confirmado: el cobro ocurrió", estadoPago)
	}

	motivo, monto := reembolsoDe(t, bd, reserva.Id)
	if motivo != "sin_cupo" {
		t.Fatalf("el reembolso quedó con motivo %q y debía ser sin_cupo", motivo)
	}
	if monto == "" {
		t.Error("el reembolso no lleva importe")
	}
}

// Una tarjeta rechazada NO cierra el intento. En Stripe deja el PaymentIntent
// listo para otro, y darlo por muerto cerraría el bucle de reintento que RF-01
// describe explícitamente.
func TestUnRechazoDeTarjetaDejaElCobroAbiertoParaReintentar(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, _ := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(13))
	if _, err := servicio.Intencion(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id); err != nil {
		t.Fatal(err)
	}
	intencionID := intencionDe(t, bd, reserva.Id)

	rechazo := eventoDeCobro("payment_intent.payment_failed", intencionID, reserva.Id)
	rechazo.Intencion.Estado = pagos.Iniciado
	rechazo.Intencion.MotivoFallo = "Tu tarjeta fue rechazada."

	if err := servicio.Procesar(ctx, rechazo); err != nil {
		t.Fatalf("el webhook falló: %v", err)
	}

	estadoReserva, estadoPago := estadoDe(t, bd, reserva.Id)
	if estadoPago != "iniciado" {
		t.Errorf("el pago quedó %q: un rechazo de tarjeta no cierra el intento", estadoPago)
	}
	if estadoReserva != "pendiente" {
		t.Errorf("la reserva quedó %q: el cupo sigue apartado hasta que venza su reloj",
			estadoReserva)
	}

	// Y el motivo llega a la interfaz, que es lo que permite decir por qué
	// falló en vez de "algo salió mal".
	confirmacion, err := servicio.Estado(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id)
	if err != nil {
		t.Fatal(err)
	}
	if confirmacion.MotivoFallo != "Tu tarjeta fue rechazada." {
		t.Errorf("el motivo del rechazo no llega a la interfaz: %q", confirmacion.MotivoFallo)
	}
}

// Una reserva ya expirada no admite pago: la interfaz tiene que enterarse antes
// de enseñar un formulario de tarjeta que no puede acabar en nada.
func TestUnaReservaExpiradaNoAdmitePago(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	servicio, _ := servicioDePrueba(t, bd)
	ctx := context.Background()

	reserva := reservaPendiente(t, bd, pruebas.Viernes(14))
	expirar(t, bd, reserva.Id)

	_, err := servicio.Intencion(ctx, uuid.MustParse(pruebas.Tenant), reserva.Id)
	if err == nil {
		t.Fatal("se abrió un cobro sobre una reserva expirada")
	}
	if !errors.Is(err, pagos.ErrReservaNoPagable) {
		t.Fatalf("el error fue %v y debía ser ErrReservaNoPagable", err)
	}
}

// ------------------------------------------------------------ afirmaciones --

func intencionDe(t *testing.T, bd *datos.BD, reserva uuid.UUID) string {
	t.Helper()

	var id string
	err := bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT payment_intent_id FROM negocio.pago
			WHERE reserva_id = $1 ORDER BY creado_en DESC LIMIT 1`, reserva).Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo leer la intención de pago: %v", err)
	}
	return id
}

func pagosDe(t *testing.T, bd *datos.BD, reserva uuid.UUID) int {
	t.Helper()
	return contar(t, bd, `SELECT count(*) FROM negocio.pago WHERE reserva_id = $1`, reserva)
}

func transiciones(t *testing.T, bd *datos.BD, reserva uuid.UUID, hasta string) int {
	t.Helper()
	return contar(t, bd, `
		SELECT count(*) FROM negocio.transicion_estado
		WHERE reserva_id = $1 AND estado_nuevo = $2::negocio.estado_reserva`, reserva, hasta)
}

func hayTransicion(t *testing.T, bd *datos.BD, reserva uuid.UUID, desde, hasta string) bool {
	t.Helper()
	return contar(t, bd, `
		SELECT count(*) FROM negocio.transicion_estado
		WHERE reserva_id = $1
		  AND estado_anterior = $2::negocio.estado_reserva
		  AND estado_nuevo    = $3::negocio.estado_reserva`, reserva, desde, hasta) > 0
}

func eventos(t *testing.T, bd *datos.BD, reserva uuid.UUID, tipo string) int {
	t.Helper()
	return contar(t, bd, `
		SELECT count(*) FROM negocio.outbox_evento
		WHERE tipo = $2 AND payload->>'reserva_id' = $1::text`, reserva, tipo)
}

func hayEvento(t *testing.T, bd *datos.BD, reserva uuid.UUID, tipo string) bool {
	t.Helper()
	return eventos(t, bd, reserva, tipo) > 0
}

func reembolsoDe(t *testing.T, bd *datos.BD, reserva uuid.UUID) (string, string) {
	t.Helper()

	var motivo, monto string
	err := bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT rb.motivo::text, rb.monto::text
			FROM negocio.reembolso rb
			JOIN negocio.pago p ON p.tenant_id = rb.tenant_id AND p.id = rb.pago_id
			WHERE p.reserva_id = $1`, reserva).Scan(&motivo, &monto)
	})
	if err != nil {
		t.Fatalf("no se encontró el reembolso: %v", err)
	}
	return motivo, monto
}

func contar(t *testing.T, bd *datos.BD, consulta string, args ...any) int {
	t.Helper()

	var n int
	err := bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(context.Background(), consulta, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("no se pudo contar: %v", err)
	}
	return n
}

// expirar simula lo que hace el expirador de RF-27 sobre una reserva concreta.
//
// Se escribe a mano en vez de invocar al trabajador porque lo que la prueba
// necesita es el ESTADO resultante, no ejercitar el barrido: eso ya lo prueba
// internal/trabajadores, sobre su propio día de la semana.
func expirar(t *testing.T, bd *datos.BD, reserva uuid.UUID) {
	t.Helper()

	ctx := context.Background()
	err := bd.EnTenant(ctx, pruebas.Tenant, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE negocio.reserva
			SET estado = 'expirada', expira_en = NULL
			WHERE id = $1`, reserva); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
			VALUES ($1::uuid, $2, 'pendiente', 'expirada', 'sistema', 'prueba')`,
			pruebas.Tenant, reserva)
		return err
	})
	if err != nil {
		t.Fatalf("no se pudo expirar la reserva: %v", err)
	}
}
