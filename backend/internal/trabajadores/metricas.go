package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// El rollup de métricas de RF-11.
//
// ER-03 lo justifica en una frase: los indicadores del requisito son agregados
// sobre dimensiones conocidas —fecha, sede, servicio— y no búsqueda ad-hoc, así
// que no hace falta un motor analítico aparte. Precalcularlos en PostgreSQL y
// servirlos desde las réplicas cubre el caso entero, y de paso hace que el
// panel del administrador no lance un GROUP BY sobre meses de reservas cada vez
// que alguien lo abre.
//
// Lo que distingue a este trabajador de los demás: no es incremental, es
// IDEMPOTENTE POR RECÁLCULO. Cada pasada recalcula la ventana entera desde las
// tablas base y sobrescribe. Un contador incremental sería más barato y estaría
// mal: una pasada perdida lo deja bajo para siempre, una repetida lo deja alto
// para siempre, y no hay forma de saber cuál de las dos cosas pasó mirando el
// número. Recalculando, cualquier fallo se corrige solo en la siguiente pasada
// y el rollup nunca puede divergir de lo que dicen las reservas.
//
// El coste de esa decisión es leer unos días de reservas cada pocos minutos,
// contra las réplicas y sobre un índice que ya existe. Es un precio pequeño por
// no tener que responder nunca "el número está mal y no sé desde cuándo".

// VentanaMetricas es cuántos días hacia atrás se recalculan en cada pasada.
//
// Siete, y no uno. Los días pasados SÍ cambian después: una reserva de ayer
// pasa a completada esta madrugada, una de la semana pasada se marca no_show,
// un reembolso corrige el ingreso. Recalcular solo hoy congelaría esos cambios
// fuera del rollup, y el panel enseñaría un pasado que no coincide con la
// agenda.
const VentanaMetricas = 7

// Metricas construye el bucle.
func Metricas(bd *datos.BD, intervalo time.Duration, registro *slog.Logger) Bucle {
	return Bucle{
		Nombre:    "metricas",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return recalcular(ctx, tx, tenant, registro)
			})
		},
	}
}

func recalcular(
	ctx context.Context, tx pgx.Tx, tenant string, registro *slog.Logger,
) (int, error) {
	// La fecha se calcula en la zona de la SEDE y no en UTC (RF-38). Un día del
	// panel es un día del negocio: una cita de las 23:00 en Bogotá pertenece a
	// ese día para quien la atiende, aunque en UTC ya sea el siguiente. Agrupar
	// por la fecha UTC repartiría las noches de cada día entre dos filas.
	//
	// El INSERT ... ON CONFLICT DO UPDATE es lo que hace el recálculo
	// idempotente: cada fila se sobrescribe entera con lo que dicen las tablas
	// base, no se le suma nada.
	etiquetas, err := tx.Exec(ctx, `
		INSERT INTO negocio.metrica_diaria AS m (
			tenant_id, fecha, sede_id, servicio_id,
			reservas_creadas, reservas_confirmadas, reservas_canceladas,
			reservas_completadas, reservas_no_show,
			ingresos, minutos_ocupados, actualizada_en
		)
		SELECT
			r.tenant_id,
			(lower(r.periodo) AT TIME ZONE sd.zona_horaria)::date AS fecha,
			sd.id, r.servicio_id,

			count(*),
			count(*) FILTER (WHERE r.estado IN
				('confirmada', 'en_curso', 'completada', 'no_show')),
			count(*) FILTER (WHERE r.estado = 'cancelada'),
			count(*) FILTER (WHERE r.estado = 'completada'),
			count(*) FILTER (WHERE r.estado = 'no_show'),

			-- El ingreso sale del PAGO confirmado y no del precio de la
			-- reserva. Son dos números distintos y la diferencia es justo lo
			-- que un panel de ingresos tiene que respetar: una reserva
			-- confirmada cuyo cobro se reembolsó no ingresó nada, y una
			-- pendiente que nadie pagó tampoco.
			coalesce(sum(pg.monto) FILTER (WHERE pg.id IS NOT NULL), 0),

			-- Los minutos ocupados cuentan lo que de verdad bloqueó el recurso.
			-- Una cancelada devolvió su hueco al mercado, así que no ocupó
			-- nada; una expirada, tampoco.
			coalesce(sum(
				EXTRACT(EPOCH FROM (upper(r.periodo) - lower(r.periodo))) / 60
			) FILTER (WHERE r.estado IN
				('pendiente', 'confirmada', 'en_curso', 'completada', 'no_show')), 0)::int,

			now()
		FROM negocio.reserva r
		JOIN negocio.recurso rc ON rc.tenant_id = r.tenant_id AND rc.id = r.recurso_id
		JOIN negocio.sede    sd ON sd.tenant_id = rc.tenant_id AND sd.id = rc.sede_id

		-- LEFT JOIN LATERAL y no un JOIN normal: una reserva puede acumular
		-- varios pagos a lo largo del tiempo —uno rechazado y otro que sí
		-- cobró— y un JOIN la duplicaría en los conteos. Aquí se toma como
		-- mucho uno: el confirmado que no se haya devuelto.
		LEFT JOIN LATERAL (
			SELECT p.id, p.monto
			FROM negocio.pago p
			LEFT JOIN negocio.reembolso rb
			  ON rb.tenant_id = p.tenant_id AND rb.pago_id = p.id
			     AND rb.estado <> 'fallido'
			WHERE p.tenant_id = r.tenant_id
			  AND p.reserva_id = r.id
			  AND p.estado = 'confirmado'
			  AND rb.id IS NULL
			LIMIT 1
		) pg ON true

		WHERE lower(r.periodo) >= (now() - make_interval(days => $1))
		  AND lower(r.periodo) <  (now() + interval '1 day')
		GROUP BY r.tenant_id, fecha, sd.id, r.servicio_id

		-- Sin cláusula WHERE en el DO UPDATE, aunque tiente ponerla para
		-- ahorrarse las escrituras que no cambian nada: actualizada_en vale
		-- now() en cada pasada, así que la fila SIEMPRE difiere y el filtro no
		-- descartaría ninguna. Sería una condición que se lee como una
		-- optimización y no optimiza nada.
		ON CONFLICT (tenant_id, fecha, sede_id, servicio_id) DO UPDATE SET
			reservas_creadas     = EXCLUDED.reservas_creadas,
			reservas_confirmadas = EXCLUDED.reservas_confirmadas,
			reservas_canceladas  = EXCLUDED.reservas_canceladas,
			reservas_completadas = EXCLUDED.reservas_completadas,
			reservas_no_show     = EXCLUDED.reservas_no_show,
			ingresos             = EXCLUDED.ingresos,
			minutos_ocupados     = EXCLUDED.minutos_ocupados,
			actualizada_en       = EXCLUDED.actualizada_en`,
		VentanaMetricas)
	if err != nil {
		return 0, err
	}

	filas := int(etiquetas.RowsAffected())
	if filas == 0 {
		return 0, nil
	}

	registro.DebugContext(ctx, "rollup de métricas actualizado",
		slog.String("tenant", tenant), slog.Int("filas", filas))

	// Se devuelve 1 por tenant recalculado, NO el número de filas escritas, y
	// la diferencia no es cosmética. El bucle vuelve enseguida cuando una
	// pasada llena su lote —señal de que queda cola— y aquí el número de filas
	// no significa eso: un tenant con muchas sedes supera LoteMaximo en una
	// sola pasada que ya lo hizo todo, y devolverlo convertiría este trabajador
	// en un bucle sin espera recalculando lo mismo para siempre.
	return 1, nil
}
