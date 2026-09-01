package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Las transiciones automáticas de RF-28.
//
// La leyenda del requisito reparte quién mueve cada estado, y solo dos no
// tienen a nadie detrás:
//
//	en_curso   -> completada   pasó la hora de fin
//	confirmada -> no_show      pasó el umbral sin que nadie registrara la llegada
//
// Sin esto, una cita de hace tres semanas sigue diciendo `confirmada` para
// siempre: nada la mueve, porque el check-in es del administrador (RF-32) y esa
// superficie todavía no existe. Con esto, acaba en `no_show`, que es lo que de
// verdad pasó.
//
// El no-show es el único de los tres que cuesta dinero equivocarse, y por eso
// su umbral es configuración y no una constante: marcar como ausente a alguien
// que llegó tarde tiene consecuencias en el reembolso de RF-29, y cuánto es
// "tarde" lo decide el negocio, no este archivo.

// Transiciones construye el bucle.
func Transiciones(bd *datos.BD, intervalo, umbralNoShow time.Duration, registro *slog.Logger) Bucle {
	return Bucle{
		Nombre:    "transiciones",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return moverEstados(ctx, tx, tenant, umbralNoShow, registro)
			})
		},
	}
}

// paso describe una transición automática: de dónde a dónde y con qué
// condición.
//
// En una tabla y no en tres funciones porque las tres hacen exactamente lo
// mismo con distinta condición, y escribirlas por separado invita a que una
// gane un detalle —una transición sin historial, un lote sin límite— que las
// otras no tienen.
type paso struct {
	desde  string
	hasta  string
	motivo string

	// condicion se evalúa sobre la fila `r` de negocio.reserva. Sus parámetros
	// son $1 = estado de origen, $2 = tamaño del lote y, solo cuando
	// usaUmbral, $3 = el umbral en segundos.
	condicion string

	// usaUmbral evita pasar un parámetro que la condición no menciona.
	// PostgreSQL infiere el tipo de cada parámetro por dónde se usa, así que
	// uno que no aparece en ninguna parte no tiene tipo que inferir y la
	// consulta ni siquiera se prepara: "could not determine data type of
	// parameter $3".
	usaUmbral bool
}

// Solo DOS, y la que falta es la importante.
//
// `confirmada -> en_curso` NO está aquí, aunque sea tentador ponerla: RF-28 la
// asigna al administrador —"check-in registrado por el administrador, RF-32"—
// y su leyenda cierra la puerta al decir que las automáticas son el no-show y
// la expiración del pago, nada más. Que el sistema marcara la llegada de alguien
// por el mero hecho de que dieran las diez sería inventarse un dato: nadie sabe
// todavía si esa persona apareció.
//
// De ahí sale también el orden. Una cita que nadie registró se queda en
// `confirmada` hasta que el umbral la convierte en `no_show`; no pasa por
// `en_curso`, porque a `en_curso` solo se llega por la mano de alguien.
var pasos = []paso{
	{
		// Lo que el administrador abrió, lo cierra el sistema: una vez que la
		// hora de fin pasó, la cita está terminada por definición y no hay
		// nada que nadie tenga que confirmar.
		desde:     "en_curso",
		hasta:     "completada",
		motivo:    "la cita terminó (RF-28)",
		condicion: "upper(r.periodo) <= now()",
	},
	{
		desde:     "confirmada",
		hasta:     "no_show",
		motivo:    "nadie registró la llegada dentro del umbral (RF-28)",
		condicion: "lower(r.periodo) + make_interval(secs => $3::float8) <= now()",
		usaUmbral: true,
	},
}

func moverEstados(
	ctx context.Context, tx pgx.Tx, tenant string, umbral time.Duration, registro *slog.Logger,
) (int, error) {
	total := 0

	for _, p := range pasos {
		// Mismo patrón que el expirador: seleccionar con SKIP LOCKED para que
		// varias réplicas se repartan la cola, y actualizar después porque hace
		// falta la lista para escribir las transiciones.
		argumentos := []any{p.desde, LoteMaximo}
		if p.usaUmbral {
			argumentos = append(argumentos, umbral.Seconds())
		}

		filas, err := tx.Query(ctx, `
			SELECT r.id::text
			FROM negocio.reserva r
			WHERE r.estado = $1::negocio.estado_reserva
			  AND `+p.condicion+`
			ORDER BY lower(r.periodo)
			LIMIT $2
			FOR UPDATE SKIP LOCKED`,
			argumentos...)
		if err != nil {
			return total, err
		}

		var afectadas []string
		for filas.Next() {
			var id string
			if err := filas.Scan(&id); err != nil {
				filas.Close()
				return total, err
			}
			afectadas = append(afectadas, id)
		}
		filas.Close()
		if err := filas.Err(); err != nil {
			return total, err
		}

		if len(afectadas) == 0 {
			continue
		}

		if _, err := tx.Exec(ctx, `
			UPDATE negocio.reserva
			SET estado = $2::negocio.estado_reserva
			WHERE id = ANY($1::uuid[])`, afectadas, p.hasta); err != nil {
			return total, err
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
			SELECT $1, id::uuid, $3::negocio.estado_reserva, $4::negocio.estado_reserva,
			       'sistema', $5
			FROM unnest($2::uuid[]) AS id`,
			tenant, afectadas, p.desde, p.hasta, p.motivo); err != nil {
			return total, err
		}

		registro.DebugContext(ctx, "transición automática aplicada",
			slog.String("tenant", tenant),
			slog.String("desde", p.desde), slog.String("hasta", p.hasta),
			slog.Int("cantidad", len(afectadas)))

		total += len(afectadas)
	}

	return total, nil
}
