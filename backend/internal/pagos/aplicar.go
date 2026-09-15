package pagos

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// El efecto de un cobro confirmado, en un solo sitio.
//
// Lo aplican DOS caminos distintos y por eso está aquí y no dentro del
// manejador del webhook:
//
//	el webhook          cuando Stripe avisa (RF-33, camino normal)
//	el conciliador      cuando Stripe NO avisó y hay que ir a preguntar
//
// Escrito dos veces, basta con que uno gane una escritura —la transición de
// RF-28, el evento del outbox— para que la misma reserva quede distinta según
// quién la confirmó, y eso no lo detecta ninguna prueba de los dos caminos por
// separado. Aquí es literalmente el mismo código.
//
// Todo ocurre dentro de la transacción que recibe. Quien la abre decide el
// alcance: el webhook una por evento, el conciliador una por tenant.

// Desenlace dice qué pasó al aplicar el cobro, para que quien llame decida.
type Desenlace int

const (
	// CobroAplicado: la reserva pasó a confirmada, o ya lo estaba.
	CobroAplicado Desenlace = iota

	// CobroSinCupo: se cobró por una reserva que ya no se puede honrar
	// —expirada, cancelada, inexistente— o el pago ni siquiera está
	// registrado. Es la rama de RF-33 que manda devolver el dinero.
	CobroSinCupo
)

// AplicarCobro marca el pago como confirmado y promueve la reserva si procede.
//
// Es idempotente: aplicarlo dos veces sobre la misma reserva no cambia nada la
// segunda vez. Tiene que serlo porque los dos caminos que lo invocan pueden
// coincidir —el webhook llega tarde justo cuando el conciliador ya preguntó— y
// porque la entrega de Stripe es at-least-once.
func AplicarCobro(
	ctx context.Context, tx pgx.Tx, tenant, reserva uuid.UUID, intencionID string,
) (Desenlace, error) {
	// El pago se marca confirmado venga del estado que venga. Puede venir de
	// 'iniciado' —el caso normal— o de 'fallido', si una tarjeta fue rechazada
	// y la siguiente sí pasó sobre el mismo PaymentIntent.
	etiquetas, err := tx.Exec(ctx, `
		UPDATE negocio.pago
		SET estado = 'confirmado', confirmado_en = now(), motivo_fallo = NULL
		WHERE payment_intent_id = $1 AND estado <> 'confirmado'`,
		intencionID)
	if err != nil {
		return CobroAplicado, err
	}

	if etiquetas.RowsAffected() == 0 {
		// O ya estaba confirmado —reentrega, y no hay nada que hacer— o la fila
		// no existe. Se distinguen consultando, porque la segunda es la rama de
		// RF-33 que dispara el reembolso: hay dinero cobrado del que este
		// sistema no tiene registro.
		var existe bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM negocio.pago WHERE payment_intent_id = $1)`,
			intencionID).Scan(&existe); err != nil {
			return CobroAplicado, err
		}
		if !existe {
			return CobroSinCupo, nil
		}
	}

	// FOR UPDATE: dos confirmaciones simultáneas de la misma reserva tienen que
	// serializarse, o las dos leen 'pendiente', las dos la promueven y quedan
	// dos transiciones para un solo cambio.
	var estado string
	err = tx.QueryRow(ctx,
		`SELECT estado::text FROM negocio.reserva WHERE id = $1 FOR UPDATE`,
		reserva).Scan(&estado)
	if errors.Is(err, pgx.ErrNoRows) {
		return CobroSinCupo, nil
	}
	if err != nil {
		return CobroAplicado, err
	}

	switch estado {
	case "pendiente":
		return CobroAplicado, confirmarReserva(ctx, tx, tenant, reserva)

	case "confirmada", "en_curso", "completada", "no_show":
		// Ya se honró, o ya pasó. Nada que hacer y nada que devolver: la
		// persona tuvo su cita, o la tuvo disponible.
		return CobroAplicado, nil

	default:
		// expirada o cancelada. Se cobró un cupo que ya no está.
		return CobroSinCupo, nil
	}
}

// confirmarReserva promueve la reserva y deja el rastro que RF-28 exige.
//
// Las tres escrituras van en la misma transacción por la razón de siempre: si
// el evento del outbox se quedara fuera, la reserva estaría confirmada, nadie
// recibiría el aviso, y no habría forma de saber que faltó.
func confirmarReserva(ctx context.Context, tx pgx.Tx, tenant, reserva uuid.UUID) error {
	// expira_en se pone a NULL. Una reserva confirmada ya no es un bloqueo con
	// reloj, y dejarlo puesto haría que el expirador de RF-27 la mirara en cada
	// pasada solo para descartarla.
	if _, err := tx.Exec(ctx, `
		UPDATE negocio.reserva
		SET estado = 'confirmada', expira_en = NULL
		WHERE id = $1`, reserva); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO negocio.transicion_estado
			(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
		VALUES ($1, $2, 'pendiente', 'confirmada', 'sistema', $3)`,
		tenant, reserva, "el pago se confirmó (RF-33)"); err != nil {
		return err
	}

	// El evento lo construye SQL y no Go, con la misma forma que emite el
	// núcleo. Es lo que permite que el notificador de RF-10 lo entienda sin
	// saber quién lo escribió.
	_, err := tx.Exec(ctx, `
		INSERT INTO negocio.outbox_evento (tenant_id, tipo, payload)
		SELECT $1, 'reserva.confirmada',
		       jsonb_build_object(
		         'reserva_id',  r.id,
		         'servicio_id', r.servicio_id,
		         'recurso_id',  r.recurso_id,
		         'estado',      r.estado,
		         'periodo', jsonb_build_object(
		            'inicio', lower(r.periodo), 'fin', upper(r.periodo)),
		         'contacto_nombre', r.contacto_nombre,
		         'contacto_email',  r.contacto_email
		       )
		FROM negocio.reserva r
		WHERE r.id = $2`, tenant, reserva)
	return err
}

// AnotarReembolso deja escrito que hay que devolver un dinero, sin devolverlo.
//
// La separación importa: escribir la fila es rápido y solo puede fallar por
// causas nuestras, mientras que devolver el dinero es una llamada a un tercero
// que puede tardar o caerse. Encadenar las dos en el manejador de un webhook
// haría que un fallo de Stripe produjera un timeout que Stripe interpretaría
// como fallo nuestro, y reintentaría el evento entero.
//
// El ON CONFLICT es la idempotencia de RF-29: una fila por pago, así que
// anotarlo dos veces no reembolsa dos veces.
func AnotarReembolso(ctx context.Context, tx pgx.Tx, intencionID, motivo string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO negocio.reembolso (tenant_id, pago_id, monto, motivo, estado)
		SELECT p.tenant_id, p.id, p.monto, $2::negocio.motivo_reembolso, 'pendiente'
		FROM negocio.pago p
		WHERE p.payment_intent_id = $1 AND p.estado = 'confirmado'
		ON CONFLICT (tenant_id, pago_id) DO NOTHING`,
		intencionID, motivo)
	return err
}

// MarcarFalloDePago guarda por qué se rechazó el último intento, sin cerrarlo.
//
// Una tarjeta rechazada NO cancela el PaymentIntent en Stripe: lo deja listo
// para otro intento. Darlo por muerto aquí cerraría el bucle de reintento que
// RF-01 describe explícitamente y obligaría a abrir una intención nueva por
// cada tarjeta que falle.
func MarcarFalloDePago(ctx context.Context, tx pgx.Tx, intencionID, motivo string) error {
	_, err := tx.Exec(ctx, `
		UPDATE negocio.pago
		SET motivo_fallo = $2
		WHERE payment_intent_id = $1 AND estado = 'iniciado'`,
		intencionID, motivo)
	return err
}

// CerrarPago marca el intento como fallido de forma definitiva.
//
// La reserva NO se toca: si su bloqueo sigue vivo, la persona todavía puede
// pedir otra intención y pagar. Quien libera el cupo es el expirador de RF-27
// cuando el reloj llegue a cero, y debe seguir siendo el único.
func CerrarPago(ctx context.Context, tx pgx.Tx, intencionID, motivo string) error {
	_, err := tx.Exec(ctx, `
		UPDATE negocio.pago
		SET estado = 'fallido', motivo_fallo = coalesce(motivo_fallo, $2)
		WHERE payment_intent_id = $1 AND estado = 'iniciado'`,
		intencionID, motivo)
	return err
}
