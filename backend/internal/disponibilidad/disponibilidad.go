// Package disponibilidad es el componente "Servicio de Disponibilidad" de
// ARQ-01: qué franjas quedan libres (RF-26).
//
// Es el 90% del tráfico (RNF-03) y una lectura optimista a propósito: puede
// devolver una franja que otro cliente acaba de tomar. Quien decide es el
// núcleo al insertar, contra la restricción EXCLUDE (RF-01, flujo alternativo
// 3). Un cliente que trate esta respuesta como un cupo garantizado está mal
// escrito.
package disponibilidad

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// VentanaMaxima acota el rango que se puede pedir de una vez.
//
// Sin cota, `desde=2020&hasta=2040` genera veinte años de franjas dentro de
// PostgreSQL antes de descartarlas: una petición barata de escribir y carísima
// de servir, que es la definición de una amplificación. Treinta y un días cubre
// el mes que muestra un calendario.
const VentanaMaxima = 31 * 24 * time.Hour

// Staleness es la desactualización que RNF-10 autoriza, y por tanto la vigencia
// exacta de lo que se guarda en el caché.
//
// No es configurable a propósito. Es un número del requisito, no del
// despliegue: subirlo rompería RNF-10 en silencio, y bajarlo tiraría por la
// borda la razón de que el caché exista. El cliente cachea lo mismo
// (`staleTime: 2_000` en consultas.ts) y por el mismo motivo.
const Staleness = 2 * time.Second

// Caché es lo que este servicio necesita de Valkey. Es una interfaz y no el
// tipo concreto para poder probar el camino sin caché y el de caché caído sin
// levantar ni apagar nada.
type Caché interface {
	Leer(ctx context.Context, clave string) ([]byte, error)
	Guardar(ctx context.Context, clave string, valor []byte, vigencia time.Duration) error
}

// Servicio calcula disponibilidad.
type Servicio struct {
	bd       *datos.BD
	cache    Caché
	registro *slog.Logger
}

// Nuevo construye el servicio. `cache` puede ser nil: entonces cada consulta va
// a PostgreSQL, que es correcto pero es exactamente lo que RNF-03 no aguanta a
// escala. Se admite nil para que las pruebas puedan medir el camino de abajo.
func Nuevo(bd *datos.BD, cache Caché, registro *slog.Logger) *Servicio {
	return &Servicio{bd: bd, cache: cache, registro: registro}
}

// ErrRangoInvalido: `hasta` no es posterior a `desde`. Un rango vacío no es
// "cero franjas", es una petición que no quiere decir nada.
var ErrRangoInvalido = errors.New("el rango pedido está vacío o invertido")

// ErrVentanaExcesiva se devuelve cuando el rango pedido supera VentanaMaxima.
type ErrVentanaExcesiva struct{ Maxima time.Duration }

func (e ErrVentanaExcesiva) Error() string {
	return "el rango pedido supera la ventana máxima de consulta"
}

// consultaFranjas es el cálculo entero, en una sola consulta.
//
// Está en SQL y no en Go porque la disponibilidad ES una resta de conjuntos
// —reglas, menos excepciones, menos reservas— y el motor ya tiene los tipos y
// los índices para hacerla: tstzrange, el operador && y el mismo índice GiST
// que sostiene la restricción EXCLUDE. Traerse las tres tablas a memoria para
// restarlas en Go significaría transferir todas las reservas del rango en cada
// una de las peticiones más frecuentes del sistema.
//
// Lo que hace, en orden:
//
//	servicio  qué se reserva, cuánto dura y en qué zona horaria está su sede
//	recursos  qué recursos pueden prestarlo (servicio_recurso)
//	dias      los días LOCALES que toca el rango pedido
//	ventanas  el horario recurrente de cada recurso, convertido a instantes
//	franjas   ese horario partido en huecos del tamaño del servicio
//
// y al final descarta las franjas ocupadas y las tapadas por una excepción.
//
// Sobre las horas locales: regla_disponibilidad guarda `time` sin zona porque
// "los lunes de 9 a 17" es una afirmación sobre el reloj de pared, no sobre un
// instante. La conversión a instante ocurre aquí, con la zona de la sede, y por
// eso sobrevive a los cambios de horario de verano: la sede sigue abriendo a
// las 9 locales aunque eso sea una hora UTC distinta en marzo y en noviembre.
//
// Sobre el pasado: una franja que ya empezó no es un hueco libre, es un hueco
// perdido. No lo impide ninguna restricción del esquema —el motor acepta
// perfectamente una reserva de ayer, y así debe ser, porque el administrador
// necesitará registrar asistencias a posteriori (RF-32)—, así que lo filtra
// esta consulta, que es la que sirve a quien está eligiendo una cita.
//
// Sobre el DISTINCT: una franja libre es una franja, la cubran una regla o
// tres. Dos reglas SOLAPADAS del mismo recurso —"lunes de 9 a 17" y "lunes de
// 13 a 15", que un administrador puede configurar sin querer— producen la misma
// franja dos veces por el join con `ventanas`, y el cliente la pintaría
// duplicada. Las reglas idénticas ya no pueden existir (regla_disponibilidad_uq,
// migración 0009); las solapadas sí, y son configuración legítima aunque
// redundante.
//
// Sobre las pendientes vencidas: solo ocupan cupo las confirmadas y las
// pendientes cuyo expira_en no ha pasado. El predicado de la restricción
// EXCLUDE no puede decir eso —PostgreSQL exige que el predicado de un índice
// sea inmutable y now() no lo es—, así que la lectura y la escritura lo
// resuelven cada una por su lado: aquí se descartan al leer, y el núcleo las
// recicla dentro de su propia transacción antes de insertar. Las dos ven lo
// mismo.
const consultaFranjas = `
WITH parametros AS (
  SELECT $1::uuid AS servicio_id,
         $2::timestamptz AS desde,
         $3::timestamptz AS hasta
),
servicio AS (
  SELECT s.id, s.sede_id, s.duracion_min, sd.zona_horaria
  FROM negocio.servicio s
  JOIN negocio.sede sd
    ON sd.tenant_id = s.tenant_id AND sd.id = s.sede_id
  CROSS JOIN parametros p
  WHERE s.id = p.servicio_id
    AND s.estado = 'activo'
    AND sd.estado = 'activo'
),
recursos AS (
  SELECT r.id
  FROM negocio.servicio_recurso sr
  JOIN negocio.recurso r
    ON r.tenant_id = sr.tenant_id AND r.id = sr.recurso_id
  CROSS JOIN servicio s
  WHERE sr.servicio_id = s.id
    AND r.estado = 'activo'
),
dias AS (
  SELECT g.momento::date AS dia
  FROM servicio s
  CROSS JOIN parametros p
  CROSS JOIN LATERAL generate_series(
    (p.desde AT TIME ZONE s.zona_horaria)::date,
    (p.hasta AT TIME ZONE s.zona_horaria)::date,
    interval '1 day'
  ) AS g(momento)
),
ventanas AS (
  SELECT rec.id AS recurso_id,
         ((d.dia + reg.hora_inicio) AT TIME ZONE s.zona_horaria) AS abre,
         ((d.dia + reg.hora_fin)    AT TIME ZONE s.zona_horaria) AS cierra
  FROM recursos rec
  JOIN negocio.regla_disponibilidad reg ON reg.recurso_id = rec.id
  CROSS JOIN dias d
  CROSS JOIN servicio s
  WHERE reg.dia_semana = extract(dow FROM d.dia)
    AND (reg.vigente_desde IS NULL OR d.dia >= reg.vigente_desde)
    AND (reg.vigente_hasta IS NULL OR d.dia <= reg.vigente_hasta)
),
franjas AS (
  SELECT v.recurso_id,
         tstzrange(g.inicio, g.inicio + make_interval(mins => s.duracion_min), '[)') AS periodo
  FROM ventanas v
  CROSS JOIN servicio s
  CROSS JOIN LATERAL generate_series(
    v.abre,
    v.cierra - make_interval(mins => s.duracion_min),
    make_interval(mins => s.duracion_min)
  ) AS g(inicio)
)
-- Con alias, y el ORDER BY sobre ellos: con SELECT DISTINCT, PostgreSQL exige
-- que lo que ordena esté en la lista de selección, y f.recurso_id a secas no lo
-- está: lo que se selecciona es su conversión a texto.
SELECT DISTINCT
       f.recurso_id::text  AS recurso_id,
       lower(f.periodo)    AS inicio,
       upper(f.periodo)    AS fin
FROM franjas f
CROSS JOIN parametros p
CROSS JOIN servicio s
WHERE f.periodo <@ tstzrange(p.desde, p.hasta, '[)')
  AND lower(f.periodo) > now()
  AND NOT EXISTS (
    SELECT 1 FROM negocio.reserva r
    WHERE r.recurso_id = f.recurso_id
      AND r.periodo && f.periodo
      AND (r.estado = 'confirmada'
           OR (r.estado = 'pendiente' AND r.expira_en > now()))
  )
  AND NOT EXISTS (
    SELECT 1 FROM negocio.excepcion_calendario e
    WHERE e.periodo && f.periodo
      AND (e.recurso_id = f.recurso_id OR e.sede_id = s.sede_id)
  )
ORDER BY inicio, recurso_id`

// Consultar devuelve las franjas libres de un servicio en [desde, hasta).
//
// Un servicio inexistente, inactivo o de otro tenant produce datos.ErrNoEncontrado
// y no una lista vacía. La diferencia importa para el cliente: "no hay huecos
// esta semana" y "ese servicio no existe" piden pantallas distintas.
func (s *Servicio) Consultar(
	ctx context.Context, tenant, servicio uuid.UUID, desde, hasta time.Time,
) (api.Disponibilidad, error) {
	if !hasta.After(desde) {
		return api.Disponibilidad{}, ErrRangoInvalido
	}
	if hasta.Sub(desde) > VentanaMaxima {
		return api.Disponibilidad{}, ErrVentanaExcesiva{Maxima: VentanaMaxima}
	}

	// El caché va delante de todo lo demás: es el 90% del tráfico de RNF-03, y
	// servirlo desde aquí es lo que hace que ese número sea sostenible contra
	// las réplicas. Un acierto no toca PostgreSQL en absoluto.
	clave := claveCache(tenant, servicio, desde, hasta)
	if guardada, hay := s.desdeCache(ctx, clave); hay {
		return guardada, nil
	}

	resultado := api.Disponibilidad{
		ServicioId: servicio,
		Franjas:    make([]api.Franja, 0),
	}

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		// El servicio se comprueba aparte y antes: la consulta grande devuelve
		// cero filas tanto si el servicio no existe como si existe y está
		// lleno, y son dos respuestas distintas.
		var existe bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM negocio.servicio s
				JOIN negocio.sede sd ON sd.tenant_id = s.tenant_id AND sd.id = s.sede_id
				WHERE s.id = $1 AND s.estado = 'activo' AND sd.estado = 'activo'
			)`, servicio).Scan(&existe); err != nil {
			return err
		}
		if !existe {
			return datos.ErrNoEncontrado
		}

		filas, err := tx.Query(ctx, consultaFranjas, servicio, desde, hasta)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var (
				recursoID string
				franja    api.Franja
			)
			if err := filas.Scan(&recursoID, &franja.Periodo.Inicio, &franja.Periodo.Fin); err != nil {
				return err
			}
			if franja.RecursoId, err = uuid.Parse(recursoID); err != nil {
				return err
			}

			resultado.Franjas = append(resultado.Franjas, franja)
		}

		return filas.Err()
	})
	if err != nil {
		return api.Disponibilidad{}, err
	}

	// Se sella después de la consulta y no antes: es la marca que el cliente
	// usa para saber cuánta desactualización está viendo, y fecharla antes
	// mentiría por el tiempo que tardó la consulta.
	resultado.CalculadaEn = time.Now().UTC()

	// Se guarda CON su calculada_en dentro. Es la diferencia entre un campo
	// útil y uno decorativo: si se sellara al servir, cada respuesta desde el
	// caché diría "recién calculado" y el cliente no podría saber que está
	// mirando algo de hace dos segundos, que es justo lo que el campo existe
	// para contarle.
	s.aCache(ctx, clave, resultado)

	return resultado, nil
}

// claveCache identifica una proyección.
//
// El tenant va SIEMPRE dentro. Es la única línea que separa una respuesta
// cacheada de una fuga entre negocios: bajo RLS la consulta no puede devolver
// filas de otro tenant, pero el caché está por encima de RLS y una clave sin
// tenant serviría lo de uno a otro sin que PostgreSQL llegara a enterarse.
//
// Los instantes van en Unix y no formateados: dos representaciones del mismo
// momento producirían dos claves para la misma pregunta.
func claveCache(tenant, servicio uuid.UUID, desde, hasta time.Time) string {
	return fmt.Sprintf("disp:%s:%s:%d:%d",
		tenant, servicio, desde.UTC().Unix(), hasta.UTC().Unix())
}

// desdeCache intenta servir sin tocar PostgreSQL.
//
// Cualquier problema —caché ausente, Valkey caído, valor ilegible— devuelve
// "no hay" y la consulta sigue contra el motor. El caché acelera; no decide, y
// no puede impedir que se responda.
func (s *Servicio) desdeCache(ctx context.Context, clave string) (api.Disponibilidad, bool) {
	if s.cache == nil {
		return api.Disponibilidad{}, false
	}

	crudo, err := s.cache.Leer(ctx, clave)
	if err != nil {
		// Un fallo de caché es el caso normal la primera vez y no merece una
		// línea de registro; un Valkey caído sí, porque significa que TODO el
		// tráfico está cayendo sobre las réplicas.
		if !errors.Is(err, cache.ErrVacio) {
			s.registro.WarnContext(ctx, "el caché de disponibilidad no responde; se sirve desde PostgreSQL",
				slog.String("error", err.Error()))
		}
		return api.Disponibilidad{}, false
	}

	var guardada api.Disponibilidad
	if err := json.Unmarshal(crudo, &guardada); err != nil {
		// Un valor ilegible es un formato viejo tras un despliegue. No es un
		// error que propagar: caduca solo en 2 s y mientras tanto se sirve
		// desde el motor.
		return api.Disponibilidad{}, false
	}

	return guardada, true
}

func (s *Servicio) aCache(ctx context.Context, clave string, resultado api.Disponibilidad) {
	if s.cache == nil {
		return
	}

	crudo, err := json.Marshal(resultado)
	if err != nil {
		return
	}

	if err := s.cache.Guardar(ctx, clave, crudo, Staleness); err != nil {
		s.registro.WarnContext(ctx, "no se pudo guardar la proyección de disponibilidad",
			slog.String("error", err.Error()))
	}
}
