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
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// VentanaMaxima acota el rango que se puede pedir de una vez.
//
// Sin cota, `desde=2020&hasta=2040` genera veinte años de franjas dentro de
// PostgreSQL antes de descartarlas: una petición barata de escribir y carísima
// de servir, que es la definición de una amplificación. Treinta y un días cubre
// el mes que muestra un calendario.
const VentanaMaxima = 31 * 24 * time.Hour

// Servicio calcula disponibilidad.
type Servicio struct {
	bd *datos.BD
}

func Nuevo(bd *datos.BD) *Servicio {
	return &Servicio{bd: bd}
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
SELECT f.recurso_id::text, lower(f.periodo), upper(f.periodo)
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
ORDER BY lower(f.periodo), f.recurso_id`

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

	return resultado, nil
}
