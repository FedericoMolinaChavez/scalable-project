package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
)

// El procesador de reembolsos de RF-29.
//
// Nadie llama a Stripe para devolver dinero desde una petición HTTP. Quien
// cancela una reserva escribe una fila en negocio.reembolso dentro de la misma
// transacción que la cancelación, y este bucle la ejecuta después. La razón es
// la de siempre en ARQ-01 —lo que depende de un tercero sale de la ruta
// síncrona— pero aquí tiene una consecuencia extra que conviene decir en voz
// alta: la respuesta de `POST /v1/reservas/{id}/cancelacion` no promete nada
// sobre el dinero, y no puede, porque en ese instante todavía no se ha movido.
//
// La idempotencia vive en dos sitios y hacen falta los dos:
//
//	UNIQUE (tenant_id, pago_id)   impide que existan dos reembolsos de un pago
//	Idempotency-Key en Stripe     impide que un reintento tras un timeout
//	                              devuelva el dinero por segunda vez
//
// El primero cubre nuestros errores; el segundo, los de la red. Este bucle
// puede morir entre llamar a Stripe y marcar la fila —es el caso normal de un
// despliegue— y sin la segunda capa la siguiente pasada reembolsaría otra vez.

// MaxIntentosReembolso es cuántas veces se reintenta antes de rendirse.
//
// Cinco, y luego la fila queda 'fallido' con su último error. Reintentar para
// siempre un reembolso que siempre falla ocupa la cola indefinidamente y
// esconde el problema: una fila fallida es visible y accionable, un bucle
// infinito no.
const MaxIntentosReembolso = 5

// Reembolsos construye el bucle.
func Reembolsos(
	bd *datos.BD, pasarela pagos.Pasarela, intervalo time.Duration, registro *slog.Logger,
) Bucle {
	return Bucle{
		Nombre:    "reembolsos",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return reembolsarLote(ctx, tx, pasarela, registro)
			})
		},
	}
}

// porDevolver es un reembolso escrito y todavía sin ejecutar.
type porDevolver struct {
	id          string
	intencionID string
	monto       string
	moneda      string
	motivo      string
	intentos    int
}

func reembolsarLote(
	ctx context.Context, tx pgx.Tx, pasarela pagos.Pasarela, registro *slog.Logger,
) (int, error) {
	filas, err := tx.Query(ctx, `
		SELECT rb.id::text, p.payment_intent_id, rb.monto::text, p.moneda,
		       rb.motivo::text, rb.intentos
		FROM negocio.reembolso rb
		JOIN negocio.pago p
		  ON p.tenant_id = rb.tenant_id AND p.id = rb.pago_id
		WHERE rb.estado = 'pendiente'
		  AND rb.intentos < $1
		ORDER BY rb.creado_en
		LIMIT $2
		FOR UPDATE OF rb SKIP LOCKED`,
		MaxIntentosReembolso, LoteMaximo)
	if err != nil {
		return 0, err
	}

	var lote []porDevolver
	for filas.Next() {
		var r porDevolver
		if err := filas.Scan(&r.id, &r.intencionID, &r.monto, &r.moneda,
			&r.motivo, &r.intentos); err != nil {
			filas.Close()
			return 0, err
		}
		lote = append(lote, r)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	devueltos := 0
	for _, pendiente := range lote {
		if ctx.Err() != nil {
			return devueltos, ctx.Err()
		}

		monto, err := pagos.DesdeTexto(pendiente.monto, pendiente.moneda)
		if err != nil {
			// Un importe que no se puede leer no se reintenta: reintentarlo
			// daría el mismo resultado para siempre. Se marca fallido con el
			// motivo, que es lo que hace que alguien lo vea.
			if err := marcarReembolso(ctx, tx, pendiente.id, pagos.ReembolsoFallido,
				"", err.Error()); err != nil {
				return devueltos, err
			}
			continue
		}

		resultado, err := pasarela.Reembolsar(ctx, pendiente.intencionID, monto, pendiente.motivo)
		if err != nil {
			// El intento se cuenta SIEMPRE, aunque el fallo sea de red. Sin el
			// contador, un reembolso imposible se reintentaría cada minuto para
			// siempre y la cola no avanzaría nunca.
			if err := contarIntento(ctx, tx, pendiente.id, err.Error()); err != nil {
				return devueltos, err
			}

			registro.WarnContext(ctx, "no se pudo reembolsar; se reintentará",
				slog.String("reembolso", pendiente.id),
				slog.Int("intentos", pendiente.intentos+1),
				slog.String("error", err.Error()))

			// Se corta el lote: si Stripe acaba de fallar, los siguientes van a
			// fallar igual, y cada intento consumido acerca a un reembolso
			// legítimo a agotarse por un problema que no es suyo.
			break
		}

		if err := marcarReembolso(ctx, tx, pendiente.id, resultado.Estado,
			resultado.ID, resultado.Error); err != nil {
			return devueltos, err
		}

		if resultado.Estado == pagos.ReembolsoConfirmado {
			devueltos++
			registro.InfoContext(ctx, "reembolso confirmado",
				slog.String("reembolso", pendiente.id),
				slog.String("motivo", pendiente.motivo))
		}
	}

	return devueltos, nil
}

// marcarReembolso escribe el desenlace.
//
// Un reembolso que sigue 'pendiente' en Stripe se queda pendiente aquí y vuelve
// a mirarse en la siguiente pasada: según el medio de pago puede tardar días, y
// darlo por bueno antes de tiempo es decirle a alguien que ya tiene su dinero
// cuando no lo tiene.
func marcarReembolso(
	ctx context.Context, tx pgx.Tx, id string, estado pagos.EstadoReembolso,
	refundID, mensaje string,
) error {
	var refund *string
	if refundID != "" {
		refund = &refundID
	}

	var ultimo *string
	if mensaje != "" {
		ultimo = &mensaje
	}

	_, err := tx.Exec(ctx, `
		UPDATE negocio.reembolso
		SET estado = $2::negocio.estado_reembolso,
		    refund_id = coalesce($3, refund_id),
		    ultimo_error = $4,
		    intentos = intentos + 1,
		    resuelto_en = CASE WHEN $2 IN ('confirmado', 'fallido') THEN now() END
		WHERE id = $1`,
		id, string(estado), refund, ultimo)
	return err
}

// contarIntento suma uno y guarda el error, sin cambiar el estado.
//
// Va en una consulta aparte de marcarReembolso porque hace algo distinto: aquí
// el reembolso sigue pendiente y solo se anota que se intentó. Mezclarlas
// obligaría a un CASE que dijera "cambia el estado salvo cuando no", que es más
// difícil de leer que dos consultas.
func contarIntento(ctx context.Context, tx pgx.Tx, id, mensaje string) error {
	_, err := tx.Exec(ctx, `
		UPDATE negocio.reembolso
		SET intentos = intentos + 1,
		    ultimo_error = $2,
		    estado = CASE WHEN intentos + 1 >= $3 THEN 'fallido'::negocio.estado_reembolso
		                  ELSE estado END,
		    resuelto_en = CASE WHEN intentos + 1 >= $3 THEN now() END
		WHERE id = $1`,
		id, mensaje, MaxIntentosReembolso)
	return err
}
