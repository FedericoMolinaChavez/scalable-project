package catalogo

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
)

// Cuándo se puede reservar: reglas semanales y excepciones (RF-14), y bajo qué
// condiciones se puede deshacer (RF-15).
//
// Estas dos superficies son las que el administrador toca con más consecuencias
// y menos retroalimentación inmediata: una regla mal puesta no falla al
// guardarse, falla cuando alguien intenta reservar o cuando nadie puede. Por
// eso lo que se puede comprobar se comprueba en el motor —el CHECK de que una
// regla no cruza medianoche, el de que una excepción es de una sede o de un
// recurso pero no de ambos— y no en un formulario.

// ErrExcepcionSinObjetivo: una excepción tapa una sede o un recurso, y
// exactamente uno.
//
// Se comprueba aquí además de en el CHECK porque el mensaje importa: el motor
// devuelve "viola una restricción" y quien lo lee tiene que poder saber qué
// falta sin abrir el esquema.
var ErrExcepcionSinObjetivo = errors.New(
	"una excepción de calendario es de una sede o de un recurso, y hace falta indicar exactamente uno")

// ------------------------------------------------------- reglas (RF-14) --

// Reglas devuelve las reglas de disponibilidad del tenant.
func (s *Servicio) Reglas(
	ctx context.Context, tenant uuid.UUID, recurso *uuid.UUID,
) ([]api.ReglaDisponibilidad, error) {
	reglas := make([]api.ReglaDisponibilidad, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, recurso_id::text, dia_semana,
			       to_char(hora_inicio, 'HH24:MI'), to_char(hora_fin, 'HH24:MI'),
			       vigente_desde, vigente_hasta
			FROM negocio.regla_disponibilidad
			WHERE ($1::uuid IS NULL OR recurso_id = $1::uuid)
			ORDER BY recurso_id, dia_semana, hora_inicio`, recurso)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			regla, err := escanearRegla(filas)
			if err != nil {
				return err
			}
			reglas = append(reglas, regla)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return reglas, nil
}

// CrearRegla publica un tramo semanal de disponibilidad (RF-14).
func (s *Servicio) CrearRegla(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nueva api.NuevaRegla,
) (api.ReglaDisponibilidad, error) {
	var regla api.ReglaDisponibilidad

	_, err := s.enTx(ctx, tenant, actor, "crear_regla", auditoria.RecursoRegla,
		func(tx pgx.Tx) (string, error) {
			// Las horas van como texto y las convierte el motor. Pasarlas por
			// un time.Time de Go obligaría a inventarles una fecha y una zona,
			// y la columna es `time` justamente porque no tiene ninguna de las
			// dos: habla del reloj de pared de la sede.
			var err error
			regla, err = escanearRegla(tx.QueryRow(ctx, `
				INSERT INTO negocio.regla_disponibilidad
					(tenant_id, recurso_id, dia_semana, hora_inicio, hora_fin,
					 vigente_desde, vigente_hasta)
				VALUES ($1, $2, $3, $4::time, $5::time, $6, $7)
				RETURNING id::text, recurso_id::text, dia_semana,
				          to_char(hora_inicio, 'HH24:MI'), to_char(hora_fin, 'HH24:MI'),
				          vigente_desde, vigente_hasta`,
				tenant, nueva.RecursoId, nueva.DiaSemana, nueva.HoraInicio, nueva.HoraFin,
				fecha(nueva.VigenteDesde), fecha(nueva.VigenteHasta)))
			if err != nil {
				return "", err
			}
			return regla.Id.String(), nil
		})
	if err != nil {
		return api.ReglaDisponibilidad{}, err
	}

	return regla, nil
}

// EliminarRegla retira un tramo (RF-14).
//
// No toca las reservas que ya se hicieron dentro de él. Una reserva confirmada
// es un compromiso; que el horario deje de publicarse no lo deshace, y
// cancelarlas sería una decisión distinta que RF-32 hace explícita.
func (s *Servicio) EliminarRegla(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, id uuid.UUID,
) error {
	_, err := s.enTx(ctx, tenant, actor, "eliminar_regla", auditoria.RecursoRegla,
		func(tx pgx.Tx) (string, error) {
			return id.String(), borrarPorID(ctx, tx, "negocio.regla_disponibilidad", id)
		})
	return err
}

// -------------------------------------------------- excepciones (RF-14) --

// Excepciones devuelve las excepciones de calendario del tenant.
func (s *Servicio) Excepciones(ctx context.Context, tenant uuid.UUID) ([]api.ExcepcionCalendario, error) {
	excepciones := make([]api.ExcepcionCalendario, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, sede_id::text, recurso_id::text,
			       lower(periodo), upper(periodo), tipo::text, motivo
			FROM negocio.excepcion_calendario
			ORDER BY lower(periodo) DESC`)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			excepcion, err := escanearExcepcion(filas)
			if err != nil {
				return err
			}
			excepciones = append(excepciones, excepcion)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return excepciones, nil
}

// CrearExcepcion tapa un período (RF-14).
func (s *Servicio) CrearExcepcion(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nueva api.NuevaExcepcion,
) (api.ExcepcionCalendario, error) {
	if (nueva.SedeId == nil) == (nueva.RecursoId == nil) {
		return api.ExcepcionCalendario{}, ErrExcepcionSinObjetivo
	}

	periodo := dominio.Periodo{Inicio: nueva.Periodo.Inicio, Fin: nueva.Periodo.Fin}
	if !periodo.Valido() {
		return api.ExcepcionCalendario{}, dominio.ErrPeriodoInvalido
	}

	var excepcion api.ExcepcionCalendario

	_, err := s.enTx(ctx, tenant, actor, "crear_excepcion", auditoria.RecursoExcepcion,
		func(tx pgx.Tx) (string, error) {
			var err error
			excepcion, err = escanearExcepcion(tx.QueryRow(ctx, `
				INSERT INTO negocio.excepcion_calendario
					(tenant_id, sede_id, recurso_id, periodo, tipo, motivo)
				VALUES ($1, $2, $3,
				        tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				        $6::negocio.tipo_excepcion, $7)
				RETURNING id::text, sede_id::text, recurso_id::text,
				          lower(periodo), upper(periodo), tipo::text, motivo`,
				tenant, nueva.SedeId, nueva.RecursoId,
				periodo.Inicio, periodo.Fin, string(nueva.Tipo), nueva.Motivo))
			if err != nil {
				return "", err
			}
			return excepcion.Id.String(), nil
		})
	if err != nil {
		return api.ExcepcionCalendario{}, err
	}

	return excepcion, nil
}

// EliminarExcepcion levanta una excepción (RF-14).
func (s *Servicio) EliminarExcepcion(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, id uuid.UUID,
) error {
	_, err := s.enTx(ctx, tenant, actor, "eliminar_excepcion", auditoria.RecursoExcepcion,
		func(tx pgx.Tx) (string, error) {
			return id.String(), borrarPorID(ctx, tx, "negocio.excepcion_calendario", id)
		})
	return err
}

// ---------------------------------------------------- políticas (RF-15) --

// Politicas devuelve las versiones publicadas, de la más reciente a la más
// antigua (RF-15).
func (s *Servicio) Politicas(ctx context.Context, tenant uuid.UUID) ([]api.Politica, error) {
	politicas := make([]api.Politica, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, servicio_id::text, version,
			       rango_cancelacion_horas, rango_modificacion_horas,
			       penalidad_pct::text, vigente_desde, creada_en
			FROM negocio.politica_version
			ORDER BY vigente_desde DESC, version DESC`)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			politica, err := escanearPolitica(filas)
			if err != nil {
				return err
			}
			politicas = append(politicas, politica)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return politicas, nil
}

// PublicarPolitica crea una versión nueva (RF-15).
//
// No hay edición ni borrado, y no es una limitación de esta implementación:
// politica_version es append-only por disparador. Publicar condiciones nuevas
// es publicar una versión nueva, y las reservas existentes siguen apuntando a
// la que congelaron. Es lo que impide que un negocio endurezca su política el
// martes y se la aplique a quien reservó el lunes.
func (s *Servicio) PublicarPolitica(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nueva api.NuevaPolitica,
) (api.Politica, error) {
	var politica api.Politica

	_, err := s.enTx(ctx, tenant, actor, "publicar_politica", auditoria.RecursoPolitica,
		func(tx pgx.Tx) (string, error) {
			// El número de versión lo calcula el motor dentro de la misma
			// transacción, no el cliente. Dejarlo fuera permitiría publicar una
			// versión 3 después de una 7 y romper el orden del que depende
			// "cuál está vigente"; calcularlo aquí lo protege además el índice
			// único, que rechaza la carrera entre dos publicaciones a la vez.
			var err error
			politica, err = escanearPolitica(tx.QueryRow(ctx, `
				INSERT INTO negocio.politica_version (
					tenant_id, servicio_id, version,
					rango_cancelacion_horas, rango_modificacion_horas,
					penalidad_pct, vigente_desde
				)
				SELECT $1, $2::uuid,
				       COALESCE(MAX(p.version), 0) + 1,
				       $3, $4, $5::numeric, COALESCE($6::timestamptz, now())
				FROM negocio.politica_version p
				WHERE p.servicio_id IS NOT DISTINCT FROM $2::uuid
				RETURNING id::text, servicio_id::text, version,
				          rango_cancelacion_horas, rango_modificacion_horas,
				          penalidad_pct::text, vigente_desde, creada_en`,
				tenant, nueva.ServicioId,
				nueva.RangoCancelacionHoras, nueva.RangoModificacionHoras,
				nueva.PenalidadPct, nueva.VigenteDesde))
			if err != nil {
				return "", err
			}
			return politica.Id.String(), nil
		})
	if err != nil {
		return api.Politica{}, err
	}

	return politica, nil
}

// ------------------------------------------------------------ auxiliares --

// borrarPorID borra una fila del tenant en curso y distingue "no había nada".
//
// Cero filas afectadas bajo RLS significa a la vez "no existe" y "es de otro
// tenant", y así debe seguir siendo: distinguirlos confirmaría qué
// identificadores son reales.
func borrarPorID(ctx context.Context, tx pgx.Tx, tabla string, id uuid.UUID) error {
	etiqueta, err := tx.Exec(ctx, "DELETE FROM "+tabla+" WHERE id = $1", id)
	if err != nil {
		return err
	}
	if etiqueta.RowsAffected() == 0 {
		return datos.ErrNoEncontrado
	}
	return nil
}

// fecha convierte la fecha del contrato en algo que pgx sepa mandar,
// conservando el nulo.
func fecha(d *openapi_types.Date) *time.Time {
	if d == nil {
		return nil
	}
	t := d.Time
	return &t
}

func escanearRegla(fila pgx.Row) (api.ReglaDisponibilidad, error) {
	var (
		regla         api.ReglaDisponibilidad
		id, recursoID string
		desde, hasta  *time.Time
	)

	if err := fila.Scan(&id, &recursoID, &regla.DiaSemana,
		&regla.HoraInicio, &regla.HoraFin, &desde, &hasta); err != nil {
		return api.ReglaDisponibilidad{}, err
	}

	var err error
	if regla.Id, err = uuid.Parse(id); err != nil {
		return api.ReglaDisponibilidad{}, err
	}
	if regla.RecursoId, err = uuid.Parse(recursoID); err != nil {
		return api.ReglaDisponibilidad{}, err
	}
	regla.VigenteDesde = comoFecha(desde)
	regla.VigenteHasta = comoFecha(hasta)

	return regla, nil
}

func comoFecha(t *time.Time) *openapi_types.Date {
	if t == nil {
		return nil
	}
	return &openapi_types.Date{Time: *t}
}

func escanearExcepcion(fila pgx.Row) (api.ExcepcionCalendario, error) {
	var (
		excepcion         api.ExcepcionCalendario
		id                string
		sedeID, recursoID *string
		tipo              string
	)

	if err := fila.Scan(&id, &sedeID, &recursoID,
		&excepcion.Periodo.Inicio, &excepcion.Periodo.Fin, &tipo, &excepcion.Motivo); err != nil {
		return api.ExcepcionCalendario{}, err
	}

	var err error
	if excepcion.Id, err = uuid.Parse(id); err != nil {
		return api.ExcepcionCalendario{}, err
	}
	if excepcion.SedeId, err = comoUUID(sedeID); err != nil {
		return api.ExcepcionCalendario{}, err
	}
	if excepcion.RecursoId, err = comoUUID(recursoID); err != nil {
		return api.ExcepcionCalendario{}, err
	}
	excepcion.Tipo = api.TipoExcepcion(tipo)

	return excepcion, nil
}

func escanearPolitica(fila pgx.Row) (api.Politica, error) {
	var (
		politica   api.Politica
		id         string
		servicioID *string
	)

	if err := fila.Scan(&id, &servicioID, &politica.Version,
		&politica.RangoCancelacionHoras, &politica.RangoModificacionHoras,
		&politica.PenalidadPct, &politica.VigenteDesde, &politica.CreadaEn); err != nil {
		return api.Politica{}, err
	}

	var err error
	if politica.Id, err = uuid.Parse(id); err != nil {
		return api.Politica{}, err
	}
	if politica.ServicioId, err = comoUUID(servicioID); err != nil {
		return api.Politica{}, err
	}

	return politica, nil
}

func comoUUID(texto *string) (*uuid.UUID, error) {
	if texto == nil {
		return nil, nil
	}
	id, err := uuid.Parse(*texto)
	if err != nil {
		return nil, err
	}
	return &id, nil
}
