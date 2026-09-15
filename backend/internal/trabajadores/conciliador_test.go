package trabajadores_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/trabajadores"
)

// El peor fallo posible de este sistema, y la prueba que lo cubre.
//
// Si un webhook de Stripe se pierde —un despliegue en el momento equivocado, un
// 502 del balanceador, una red partida— pasa esto: la tarjeta se cobró, el
// expirador de RF-27 libera el cupo a los quince minutos, y la persona se queda
// sin cita y sin dinero. Desde dentro no se distingue de una reserva que nadie
// pagó, así que ninguna otra capa lo detecta.
//
// La leyenda de RF-33 pide exactamente el remedio que se prueba aquí: consultar
// activamente al proveedor antes de liberar el horario.

// pasarelaDe devuelve siempre el mismo estado, que es lo que la prueba fija
// para representar "esto es lo que Stripe diría si se le preguntara".
type pasarelaDe struct {
	estado     pagos.Estado
	cancelados []string
}

func (p *pasarelaDe) CrearIntencion(context.Context, pagos.Solicitud) (pagos.Intencion, error) {
	return pagos.Intencion{}, nil
}

func (p *pasarelaDe) ConsultarIntencion(_ context.Context, id string) (pagos.Intencion, error) {
	return pagos.Intencion{ID: id, Estado: p.estado}, nil
}

func (p *pasarelaDe) CancelarIntencion(_ context.Context, id string) error {
	p.cancelados = append(p.cancelados, id)
	return nil
}

func (p *pasarelaDe) Reembolsar(
	context.Context, string, pagos.Monto, string,
) (pagos.Reembolso, error) {
	return pagos.Reembolso{Estado: pagos.ReembolsoConfirmado}, nil
}

func (p *pasarelaDe) ClavePublicable() string { return "pk_de_prueba" }

func (p *pasarelaDe) VerificarEvento([]byte, string) (pagos.Evento, error) {
	panic("el conciliador nunca verifica firmas: no recibe eventos, pregunta")
}

// sembrarPagoIniciado deja una fila de pago abierta sobre una reserva, con la
// antigüedad que se le diga, para caer del lado que la prueba quiere de la
// ventana de gracia.
func sembrarPagoIniciado(t *testing.T, bd *datos.BD, reservaID string, antiguedad time.Duration) string {
	t.Helper()

	intencion := "pi_prueba_" + reservaID[:8] + "_" + time.Now().Format("150405.000000")

	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		_, err := tx.Exec(t.Context(), `
			INSERT INTO negocio.pago
				(tenant_id, reserva_id, proveedor, payment_intent_id,
				 monto, moneda, estado, creado_en)
			VALUES ($1::uuid, $2::uuid, 'stripe', $3,
			        80000.00, 'COP', 'iniciado', now() - make_interval(secs => $4::float8))`,
			pruebas.Tenant, reservaID, intencion, antiguedad.Seconds())
		return err
	}); err != nil {
		t.Fatalf("no se pudo sembrar el pago: %v", err)
	}

	return intencion
}

func estadoDelPago(t *testing.T, bd *datos.BD, intencion string) string {
	t.Helper()

	var estado string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT estado::text FROM negocio.pago WHERE payment_intent_id = $1`,
			intencion).Scan(&estado)
	}); err != nil {
		t.Fatalf("no se pudo leer el pago: %v", err)
	}
	return estado
}

func estadoDeLaReserva(t *testing.T, bd *datos.BD, id string) string {
	t.Helper()

	var estado string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			`SELECT estado::text FROM negocio.reserva WHERE id = $1`, id).Scan(&estado)
	}); err != nil {
		t.Fatalf("no se pudo leer la reserva: %v", err)
	}
	return estado
}

// El caso que justifica todo el trabajador: Stripe cobró y su aviso no llegó.
func TestElConciliadorConfirmaUnPagoCuyoWebhookNuncaLlego(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(14)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	reserva := sembrarPendiente(t, bd, inicio, false)
	intencion := sembrarPagoIniciado(t, bd, reserva, 10*time.Minute)

	// Lo que Stripe contestaría: el cobro salió bien y nadie se enteró.
	pasarela := &pasarelaDe{estado: pagos.Confirmado}

	bucle := trabajadores.Conciliador(bd, pasarela, time.Second, 2*time.Minute, mudo())
	if _, err := bucle.Pasada(context.Background()); err != nil {
		t.Fatalf("la pasada falló: %v", err)
	}

	if estado := estadoDeLaReserva(t, bd, reserva); estado != "confirmada" {
		t.Errorf("la reserva quedó %q: el pago existía y nadie la confirmó", estado)
	}
	if estado := estadoDelPago(t, bd, intencion); estado != "confirmado" {
		t.Errorf("el pago quedó %q y debía quedar confirmado", estado)
	}
}

// La ventana de gracia. Preguntar por un cobro que se abrió hace dos segundos
// garantiza que la respuesta sea "todavía nada", y gasta una llamada a un
// tercero con cuota para no enterarse de nada.
func TestElConciliadorNoPreguntaPorLoQueAcabaDeEmpezar(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(15)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	reserva := sembrarPendiente(t, bd, inicio, false)
	intencion := sembrarPagoIniciado(t, bd, reserva, 5*time.Second)

	// Aunque Stripe diría que está cobrado, este pago no se mira todavía.
	pasarela := &pasarelaDe{estado: pagos.Confirmado}

	bucle := trabajadores.Conciliador(bd, pasarela, time.Second, 2*time.Minute, mudo())
	if _, err := bucle.Pasada(context.Background()); err != nil {
		t.Fatalf("la pasada falló: %v", err)
	}

	if estado := estadoDelPago(t, bd, intencion); estado != "iniciado" {
		t.Errorf("el pago quedó %q: se concilió antes de la ventana de gracia", estado)
	}
	if estado := estadoDeLaReserva(t, bd, reserva); estado != "pendiente" {
		t.Errorf("la reserva quedó %q y debía seguir pendiente", estado)
	}
}

// El trabajo simétrico: cerrar en Stripe lo que ya no puede acabar en nada.
//
// Un PaymentIntent abandonado sigue siendo cobrable durante días desde una
// pestaña que nadie cerró, y cobrarlo entonces solo puede acabar en un
// reembolso. El orden importa: se cancela allí ANTES de marcarlo aquí.
func TestElConciliadorCierraElIntentoDeUnBloqueoQueYaVencio(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(16)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	// vencida: el expirador de RF-27 ya se la llevó, o está a punto.
	reserva := sembrarPendiente(t, bd, inicio, true)
	intencion := sembrarPagoIniciado(t, bd, reserva, 20*time.Minute)

	pasarela := &pasarelaDe{estado: pagos.Iniciado}

	bucle := trabajadores.Conciliador(bd, pasarela, time.Second, 2*time.Minute, mudo())
	if _, err := bucle.Pasada(context.Background()); err != nil {
		t.Fatalf("la pasada falló: %v", err)
	}

	if estado := estadoDelPago(t, bd, intencion); estado != "fallido" {
		t.Errorf("el pago quedó %q y debía cerrarse como fallido", estado)
	}

	// Y sobre todo: se le dijo a Stripe. Marcarlo solo en nuestra base dejaría
	// un intento cobrable allí y muerto aquí, que es la combinación que produce
	// cobros que después nadie sabe explicar.
	if len(pasarela.cancelados) != 1 || pasarela.cancelados[0] != intencion {
		t.Errorf("no se canceló la intención en el proveedor: %v", pasarela.cancelados)
	}
}
