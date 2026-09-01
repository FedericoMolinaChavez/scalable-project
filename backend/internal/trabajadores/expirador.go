package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// El expirador de RF-27: devuelve al mercado los cupos que nadie pagó.
//
// Existe por una limitación concreta del motor, no por gusto. El predicado de
// la restricción EXCLUDE dice `estado IN ('pendiente','confirmada')` y NO puede
// decir "y expira_en > now()": PostgreSQL exige que el predicado de un índice
// sea inmutable, y now() no lo es —el índice tendría que reevaluarse solo, cosa
// que ningún índice hace—. La consecuencia es que un bloqueo vencido sigue
// ocupando el cupo hasta que alguien cambie su estado.
//
// Ese alguien es esto. Y su frecuencia no es un detalle operativo: es
// literalmente cuánto tiempo un cupo libre parece ocupado.
//
// El núcleo recicla además los vencidos de la franja concreta que va a insertar,
// y eso NO lo sustituye: cubre la ventana entre dos pasadas para quien pide justo
// esa franja. Lo que el núcleo no puede hacer es liberar el cupo para que otros
// lo VEAN libre en la agenda, porque nadie lo está pidiendo todavía.

// Expirador construye el bucle.
func Expirador(bd *datos.BD, intervalo time.Duration, registro *slog.Logger) Bucle {
	return Bucle{
		Nombre:    "expirador",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return expirarLote(ctx, tx, tenant, registro)
			})
		},
	}
}

func expirarLote(ctx context.Context, tx pgx.Tx, tenant string, registro *slog.Logger) (int, error) {
	// FOR UPDATE SKIP LOCKED es lo que hace que esto escale a varias réplicas.
	//
	// Sin SKIP LOCKED, dos trabajadores seleccionan las mismas filas y el
	// segundo se queda esperando al primero: el trabajo se serializa y añadir
	// réplicas no acelera nada, solo consume conexiones. Con SKIP LOCKED, cada
	// uno se lleva un lote distinto y se reparten la cola sin coordinarse, que
	// es la misma filosofía que el resto del sistema.
	//
	// Se seleccionan primero y se actualizan después, en vez de un UPDATE
	// directo, porque hace falta la lista de identificadores para escribir sus
	// transiciones.
	filas, err := tx.Query(ctx, `
		SELECT id::text
		FROM negocio.reserva
		WHERE estado = 'pendiente'
		  AND expira_en <= now()
		ORDER BY expira_en
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, LoteMaximo)
	if err != nil {
		return 0, err
	}

	var vencidas []string
	for filas.Next() {
		var id string
		if err := filas.Scan(&id); err != nil {
			filas.Close()
			return 0, err
		}
		vencidas = append(vencidas, id)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	if len(vencidas) == 0 {
		return 0, nil
	}

	// 'expirada' y no 'cancelada'. La leyenda de RF-28 dice cancelada, pero el
	// enum y la migración 0006 distinguen las dos, y la distinción importa:
	// cancelar es un acto de alguien —queda en el historial como tal, y dispara
	// la política de RF-15 y el reembolso de RF-29— mientras que expirar es que
	// no pasó nada. Confundirlas haría que un bloqueo abandonado apareciera en
	// las métricas del negocio como una cancelación de cliente.
	if _, err := tx.Exec(ctx, `
		UPDATE negocio.reserva
		SET estado = 'expirada', expira_en = NULL
		WHERE id = ANY($1::uuid[])`, vencidas); err != nil {
		return 0, err
	}

	// La historia de RF-28, en la misma transacción que el cambio.
	if _, err := tx.Exec(ctx, `
		INSERT INTO negocio.transicion_estado
			(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
		SELECT $1, id::uuid, 'pendiente', 'expirada', 'sistema', $3
		FROM unnest($2::uuid[]) AS id`,
		tenant, vencidas, "el bloqueo venció sin pago (RF-27)"); err != nil {
		return 0, err
	}

	// Y el evento, también aquí dentro: el cupo vuelve a estar libre y hay
	// quien quiere enterarse —la lista de espera de RF-37, cuando exista—.
	if _, err := tx.Exec(ctx, `
		INSERT INTO negocio.outbox_evento (tenant_id, tipo, payload)
		SELECT $1, 'reserva.expirada',
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
		WHERE r.id = ANY($2::uuid[])`,
		tenant, vencidas); err != nil {
		return 0, err
	}

	registro.DebugContext(ctx, "bloqueos expirados",
		slog.String("tenant", tenant), slog.Int("cantidad", len(vencidas)))

	return len(vencidas), nil
}
