package trabajadores

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
)

// El conciliador de pagos de RF-33.
//
// Existe por una frase concreta de la leyenda del requisito: «si el webhook no
// llega dentro del TTL del bloqueo, el sistema consulta activamente el estado
// de la transacción al proveedor antes de liberar el horario, para no perder un
// pago efectivamente cobrado».
//
// Esa frase describe el peor fallo que este sistema puede tener. Sin este
// bucle, un webhook que se pierde produce exactamente esto: la tarjeta se
// cobró, el expirador de RF-27 libera el cupo a los quince minutos, y la
// persona se queda sin cita y sin dinero. No hay ninguna otra capa que lo
// detecte, porque desde dentro no se distingue de una reserva que nadie pagó.
//
// La entrega de webhooks de Stripe es fiable, pero fiable no es seguro: un
// despliegue en el momento equivocado, un balanceador que devuelve 502, una red
// partida. El coste de este bucle es una consulta a Stripe por pago que lleve
// demasiado tiempo esperando, y el camino normal —el webhook llega en
// segundos— no produce ninguna.
//
// Hace además el trabajo simétrico, que RF-33 no pide pero se sigue de RF-27:
// cerrar en Stripe los intentos de las reservas que ya expiraron. Un
// PaymentIntent abandonado sigue siendo cobrable durante días desde una pestaña
// que nadie cerró, y cobrarlo entonces solo puede acabar en un reembolso.

// Conciliador construye el bucle.
func Conciliador(
	bd *datos.BD, pasarela pagos.Pasarela, intervalo, gracia time.Duration, registro *slog.Logger,
) Bucle {
	return Bucle{
		Nombre:    "conciliador",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return conciliarLote(ctx, tx, tenant, pasarela, gracia, registro)
			})
		},
	}
}

// enEspera es un pago iniciado del que no se ha vuelto a saber.
type enEspera struct {
	intencionID string
	reservaID   uuid.UUID
	// reservaViva dice si el bloqueo todavía no venció. Decide si el intento se
	// concilia o se cierra.
	reservaViva bool
}

func conciliarLote(
	ctx context.Context, tx pgx.Tx, tenant string,
	pasarela pagos.Pasarela, gracia time.Duration, registro *slog.Logger,
) (int, error) {
	tenantID, err := uuid.Parse(tenant)
	if err != nil {
		return 0, err
	}

	// Solo los que llevan más de la gracia esperando. Preguntar por un pago que
	// se abrió hace dos segundos garantiza que la respuesta sea "todavía nada",
	// y gasta una llamada a un tercero con cuota para no enterarse de nada.
	//
	// SKIP LOCKED como en el resto de los barridos: varias réplicas se reparten
	// la cola sin coordinarse. Aquí importa más que en ningún otro sitio, porque
	// sin él dos réplicas preguntarían por los MISMOS pagos y duplicarían el
	// tráfico hacia Stripe.
	filas, err := tx.Query(ctx, `
		SELECT p.payment_intent_id, p.reserva_id::text,
		       (r.estado = 'pendiente' AND r.expira_en > now())
		FROM negocio.pago p
		JOIN negocio.reserva r
		  ON r.tenant_id = p.tenant_id AND r.id = p.reserva_id
		WHERE p.estado = 'iniciado'
		  AND p.creado_en <= now() - make_interval(secs => $1::float8)
		ORDER BY p.creado_en
		LIMIT $2
		FOR UPDATE OF p SKIP LOCKED`,
		gracia.Seconds(), LoteMaximo)
	if err != nil {
		return 0, err
	}

	var lote []enEspera
	for filas.Next() {
		var (
			p  enEspera
			id string
		)
		if err := filas.Scan(&p.intencionID, &id, &p.reservaViva); err != nil {
			filas.Close()
			return 0, err
		}
		if p.reservaID, err = uuid.Parse(id); err != nil {
			filas.Close()
			return 0, err
		}
		lote = append(lote, p)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	conciliados := 0
	for _, pendiente := range lote {
		if ctx.Err() != nil {
			return conciliados, ctx.Err()
		}

		hecho, err := conciliarUno(ctx, tx, tenantID, pendiente, pasarela, registro)
		if err != nil {
			// Se corta la pasada y se conserva lo hecho. Seguir preguntando a
			// un Stripe que acaba de fallar solo alarga el fallo, y la
			// transacción confirma lo que sí se pudo conciliar: quien se quedó
			// fuera entra en la siguiente pasada.
			registro.WarnContext(ctx, "la conciliación se detuvo; se reintenta en la siguiente pasada",
				slog.String("intencion", pendiente.intencionID),
				slog.String("error", err.Error()))
			break
		}
		if hecho {
			conciliados++
		}
	}

	return conciliados, nil
}

// conciliarUno resuelve un pago preguntándole al proveedor.
func conciliarUno(
	ctx context.Context, tx pgx.Tx, tenant uuid.UUID, pendiente enEspera,
	pasarela pagos.Pasarela, registro *slog.Logger,
) (bool, error) {
	// El bloqueo ya venció y el cupo se liberó (o está a punto). No hay nada
	// que confirmar: lo que toca es cerrar el intento en Stripe para que nadie
	// pueda cobrarlo después.
	//
	// El orden es "cancelar allí, marcar aquí". Al revés dejaría un intento
	// cobrable en Stripe marcado como muerto en nuestra base, que es la
	// combinación que produce cobros que no se pueden explicar.
	if !pendiente.reservaViva {
		if err := pasarela.CancelarIntencion(ctx, pendiente.intencionID); err != nil &&
			!errors.Is(err, pagos.ErrIntencionNoEncontrada) {
			return false, err
		}

		registro.InfoContext(ctx, "intento de pago cerrado: el bloqueo ya no está vivo",
			slog.String("intencion", pendiente.intencionID))

		return true, pagos.CerrarPago(ctx, tx, pendiente.intencionID,
			"el bloqueo venció antes de completarse el pago (RF-27)")
	}

	intencion, err := pasarela.ConsultarIntencion(ctx, pendiente.intencionID)
	if errors.Is(err, pagos.ErrIntencionNoEncontrada) {
		// Nuestra fila apunta a algo que el proveedor no conoce. Se cierra para
		// que la persona pueda pedir otra intención y pagar: dejarla viva la
		// dejaría atascada contra un identificador que no existe.
		return true, pagos.CerrarPago(ctx, tx, pendiente.intencionID,
			"el proveedor no reconoce esta intención de pago")
	}
	if err != nil {
		return false, err
	}

	switch intencion.Estado {
	case pagos.Confirmado:
		// El webhook no llegó y el dinero SÍ. Este es el caso por el que existe
		// todo este archivo.
		registro.WarnContext(ctx, "pago confirmado sin que llegara su webhook; se concilia",
			slog.String("intencion", intencion.ID),
			slog.String("reserva", pendiente.reservaID.String()))

		desenlace, err := pagos.AplicarCobro(ctx, tx, tenant, pendiente.reservaID, intencion.ID)
		if err != nil {
			return false, err
		}
		if desenlace == pagos.CobroSinCupo {
			return true, pagos.AnotarReembolso(ctx, tx, intencion.ID, "sin_cupo")
		}
		return true, nil

	case pagos.Fallido:
		return true, pagos.CerrarPago(ctx, tx, intencion.ID, motivoO(intencion.MotivoFallo,
			"el proveedor dio el intento por cancelado"))

	default:
		// Sigue abierto. No es un fallo ni hay nada que hacer: la persona
		// todavía está pagando, o todavía no ha empezado. Lo único que se
		// guarda es el último rechazo, si lo hubo, porque es lo que la interfaz
		// enseña para que se pueda reintentar con otra tarjeta.
		if intencion.MotivoFallo != "" {
			return false, pagos.MarcarFalloDePago(ctx, tx, intencion.ID, intencion.MotivoFallo)
		}
		return false, nil
	}
}

func motivoO(motivo, porDefecto string) string {
	if motivo == "" {
		return porDefecto
	}
	return motivo
}
