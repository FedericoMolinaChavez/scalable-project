package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// La lista de espera de RF-37.
//
// Cuando un cupo se libera —el bloqueo venció (RF-27), alguien canceló
// (RF-06)— hay quien lo estaba esperando, y RF-37 dice cómo se reparte: por
// orden de creada_en, no por prioridad ni por quién pague más. Es lo único que
// una persona acepta como justo sin que haya que explicárselo.
//
// La forma de este trabajador es distinta de la de los demás, y por una razón
// que conviene ver. Los otros barren un estado ("pendientes vencidas", "pagos
// sin conciliar"); este barre una AUSENCIA: franjas donde ya no hay reserva
// viva y sí hay alguien anotado. No hay una columna que diga "este cupo se
// acaba de liberar", y no debería haberla: sería un estado derivado que habría
// que mantener sincronizado con la tabla de reservas, y el día que se
// desincronizara nadie se enteraría.
//
// Notifica y NO reserva. Reservar por su cuenta le daría a alguien una cita que
// no pidió —y un cobro que no autorizó—; lo que hace es avisar y dejar la franja
// libre para quien llegue primero, incluido quien no estaba en la lista. Marcar
// 'notificada' es lo que impide avisar dos veces por lo mismo.
//
// DEPENDE DE RF-12 PARA HACER ALGO. negocio.lista_espera.cuenta_id es NOT NULL
// y referencia plataforma.cuenta, así que hoy nadie puede anotarse: no hay
// cuentas. Este bucle corre y no encuentra filas. Está escrito igualmente
// porque es uno de los ocho trabajadores que ARQ-01 nombra y porque su lógica
// —qué es un cupo liberado, a quién le toca— es independiente de cómo se
// autentique quien espera.

// Espera construye el bucle.
func Espera(bd *datos.BD, intervalo time.Duration, registro *slog.Logger) Bucle {
	return Bucle{
		Nombre:    "lista-de-espera",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return avisarLote(ctx, tx, tenant, registro)
			})
		},
	}
}

func avisarLote(
	ctx context.Context, tx pgx.Tx, tenant string, registro *slog.Logger,
) (int, error) {
	// Quién espera por una franja que ahora mismo está libre, y a quién le toca.
	//
	// "Libre" se define exactamente igual que en el servicio de disponibilidad:
	// no hay reserva que la solape en un estado que ocupe cupo. Las pendientes
	// vencidas NO ocupan —es la misma reconciliación que hacen el núcleo y el
	// expirador— porque si no, un bloqueo abandonado seguiría escondiendo el
	// hueco de quien lleva días esperándolo.
	//
	// El turno de RF-37 lo resuelve row_number() por franja, ordenado por
	// creada_en: gana quien se anotó antes. Sin él se avisaría a todos a la vez
	// y la lista dejaría de ser una lista.
	//
	// Y aquí NO hay FOR UPDATE SKIP LOCKED, al revés que en los demás barridos:
	// PostgreSQL no admite bloqueo de filas junto a DISTINCT ni a funciones de
	// ventana ("FOR UPDATE is not allowed with DISTINCT clause"). El reparto
	// entre réplicas lo hace el propio UPDATE: su `estado = 'en_espera'` es la
	// condición de carrera, y RETURNING devuelve solo las filas que ESTA
	// transacción ganó. Dos trabajadores sobre la misma fila producen un
	// ganador y un cero, que es la misma garantía por otro camino.
	filas, err := tx.Query(ctx, `
		WITH ordenadas AS (
			SELECT e.id,
			       row_number() OVER (
			         PARTITION BY e.servicio_id, e.periodo ORDER BY e.creada_en
			       ) AS turno
			FROM negocio.lista_espera e
			JOIN plataforma.cuenta c ON c.id = e.cuenta_id
			WHERE e.estado = 'en_espera'
			  -- Sin correo no hay a quién avisar. La columna es nullable porque
			  -- una cuenta puede identificarse solo por teléfono (RF-19), y el
			  -- SMS todavía no existe: avisar por un canal que no está montado
			  -- dejaría la fila marcada como notificada sin que nadie se
			  -- enterara.
			  AND c.email IS NOT NULL
			  AND upper(e.periodo) > now()
			  AND NOT EXISTS (
			        SELECT 1
			        FROM negocio.reserva r
			        WHERE r.tenant_id = e.tenant_id
			          AND r.servicio_id = e.servicio_id
			          AND (e.recurso_id IS NULL OR r.recurso_id = e.recurso_id)
			          AND r.periodo && e.periodo
			          AND (r.estado = 'confirmada'
			               OR (r.estado = 'pendiente' AND r.expira_en > now())))
		),
		candidatas AS (
			SELECT id FROM ordenadas WHERE turno = 1 LIMIT $1
		)
		UPDATE negocio.lista_espera e
		SET estado = 'notificada'
		FROM candidatas
		WHERE e.id = candidatas.id
		  AND e.estado = 'en_espera'
		RETURNING e.id::text`, LoteMaximo)
	if err != nil {
		return 0, err
	}

	var avisadas []string
	for filas.Next() {
		var id string
		if err := filas.Scan(&id); err != nil {
			filas.Close()
			return 0, err
		}
		avisadas = append(avisadas, id)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	if len(avisadas) == 0 {
		return 0, nil
	}

	// El evento va en la MISMA transacción que el cambio de estado. Es lo mismo
	// que hacen el expirador y el núcleo: si se emitiera fuera, una caída entre
	// ambos dejaría a alguien marcado como avisado sin que le llegara nada, y
	// nadie podría saber que faltó.
	//
	// Su payload tiene la forma que el notificador de RF-10 ya entiende
	// —contacto y período— aunque el asunto sea otro.
	if _, err := tx.Exec(ctx, `
		INSERT INTO negocio.outbox_evento (tenant_id, tipo, payload)
		SELECT e.tenant_id, 'espera.cupo_libre',
		       jsonb_build_object(
		         'espera_id', e.id,
		         'servicio',  s.nombre,
		         'periodo', jsonb_build_object(
		            'inicio', lower(e.periodo), 'fin', upper(e.periodo)),
		         'contacto_nombre', coalesce(c.nombre, ''),
		         'contacto_email',  c.email
		       )
		FROM negocio.lista_espera e
		JOIN negocio.servicio s
		  ON s.tenant_id = e.tenant_id AND s.id = e.servicio_id
		JOIN plataforma.cuenta c ON c.id = e.cuenta_id
		WHERE e.id = ANY($1::uuid[])`, avisadas); err != nil {
		return 0, err
	}

	registro.InfoContext(ctx, "cupos libres avisados a la lista de espera",
		slog.String("tenant", tenant), slog.Int("cantidad", len(avisadas)))

	return len(avisadas), nil
}
