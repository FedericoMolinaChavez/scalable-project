package catalogo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// La mitad de escritura del componente "Configuración y Catálogo" de ARQ-01:
// RF-30 (sedes, servicios y recursos), y con ella el patrón que siguen también
// RF-14, RF-15, RF-17 y RF-31.
//
// Todo lo que escribe pasa por `enTx`, y eso no es un atajo de estilo: RNF-36
// hace de la auditoría una CONDICIÓN DE ÉXITO, así que la fila de
// evento_auditoria tiene que caber en la misma transacción que el cambio. Un
// método que escribiera por su cuenta podría olvidarse, y el olvido sería
// invisible: la operación funcionaría igual, solo que sin dejar rastro.

// ErrNoEncontrado se reexporta para que quien use este paquete no tenga que
// importar datos solo para comparar un error.
var ErrNoEncontrado = datos.ErrNoEncontrado

// enTx ejecuta una escritura y la audita dentro de la MISMA transacción.
//
// `fn` devuelve el identificador de la fila afectada, que es lo que la
// auditoría necesita para poder responder "qué se tocó".
//
// Cuando `fn` falla, el rechazo se audita en una transacción APARTE, y tiene
// que ser así: la primera ya está abortada y no admite ni un INSERT más. No
// rompe ninguna garantía porque una acción rechazada no ocurrió —no hay nada
// con lo que ser atómico— y el rechazo sigue siendo un hecho que RF-36 quiere
// registrado. Si esa segunda escritura también falla, se propaga el error
// original: lo que le importa a quien llamó es que su acción no se hizo.
func (s *Servicio) enTx(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor,
	accion, recursoTipo string, fn func(tx pgx.Tx) (string, error),
) (string, error) {
	var id string

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var err error
		if id, err = fn(tx); err != nil {
			return err
		}

		return auditoria.Escribir(ctx, tx, tenant, actor, auditoria.Evento{
			Accion:      accion,
			RecursoTipo: recursoTipo,
			RecursoID:   id,
			Resultado:   auditoria.Exito,
		})
	})
	if err != nil {
		s.auditarRechazo(ctx, tenant, actor, accion, recursoTipo, id)
		return "", err
	}

	return id, nil
}

func (s *Servicio) auditarRechazo(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor,
	accion, recursoTipo, recursoID string,
) {
	// El error se descarta a propósito y es el único sitio del paquete donde
	// eso es correcto: la operación YA falló y ya se le va a devolver un error
	// a quien llamó. Sustituirlo por el de la auditoría del rechazo diría que
	// falló por un motivo que no es el suyo.
	_ = s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return auditoria.Escribir(ctx, tx, tenant, actor, auditoria.Evento{
			Accion:      accion,
			RecursoTipo: recursoTipo,
			RecursoID:   recursoID,
			Resultado:   auditoria.Rechazo,
		})
	})
}

// ------------------------------------------------------------------ sedes --

// SedesTodas devuelve las sedes del tenant, activas e inactivas (RF-30).
//
// Es una función aparte de Sedes y no un parámetro suyo. Aquella la consume el
// cliente final y solo debe devolver lo activo; mezclarlas en una con un
// booleano deja una función cuyo comportamiento por defecto decide quién la
// llama, y el día que alguien se equivoque de valor, el catálogo público
// enseñará sedes cerradas.
func (s *Servicio) SedesTodas(ctx context.Context, tenant uuid.UUID) ([]api.Sede, error) {
	return s.leerSedes(ctx, tenant, false)
}

func (s *Servicio) leerSedes(ctx context.Context, tenant uuid.UUID, soloActivas bool) ([]api.Sede, error) {
	sedes := make([]api.Sede, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, nombre, zona_horaria, direccion, estado::text
			FROM negocio.sede
			WHERE NOT $1::boolean OR estado = 'activo'
			ORDER BY nombre`, soloActivas)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			sede, err := escanearSede(filas)
			if err != nil {
				return err
			}
			sedes = append(sedes, sede)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return sedes, nil
}

// CrearSede da de alta una sede (RF-30).
func (s *Servicio) CrearSede(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nueva api.NuevaSede,
) (api.Sede, error) {
	var sede api.Sede

	_, err := s.enTx(ctx, tenant, actor, "crear_sede", auditoria.RecursoSede,
		func(tx pgx.Tx) (string, error) {
			// La zona horaria la valida el motor contra su propia base de
			// zonas: un `America/Bogata` mal escrito no falla al guardarse,
			// falla semanas más tarde dentro del cálculo de disponibilidad.
			var err error
			sede, err = escanearSede(tx.QueryRow(ctx, `
				INSERT INTO negocio.sede (tenant_id, nombre, zona_horaria, direccion)
				VALUES ($1, $2, $3, $4)
				RETURNING id::text, nombre, zona_horaria, direccion, estado::text`,
				tenant, nueva.Nombre, nueva.ZonaHoraria, nueva.Direccion))
			if err != nil {
				return "", err
			}
			return sede.Id.String(), nil
		})
	if err != nil {
		return api.Sede{}, err
	}

	return sede, nil
}

// ActualizarSede edita una sede (RF-30).
//
// COALESCE por campo: un PATCH que solo trae el nombre no puede borrar la
// dirección. Desactivarla NO cancela sus reservas —eso es una decisión aparte
// que RF-32 hace explícita— sino que deja de poder elegirse para reservas
// nuevas.
func (s *Servicio) ActualizarSede(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor,
	id uuid.UUID, cambio api.ActualizacionSede,
) (api.Sede, error) {
	var sede api.Sede

	_, err := s.enTx(ctx, tenant, actor, "actualizar_sede", auditoria.RecursoSede,
		func(tx pgx.Tx) (string, error) {
			var err error
			sede, err = escanearSede(tx.QueryRow(ctx, `
				UPDATE negocio.sede
				SET nombre       = COALESCE($2::text, nombre),
				    zona_horaria = COALESCE($3::text, zona_horaria),
				    direccion    = COALESCE($4::text, direccion),
				    estado       = COALESCE($5::negocio.estado_catalogo, estado)
				WHERE id = $1
				RETURNING id::text, nombre, zona_horaria, direccion, estado::text`,
				id, cambio.Nombre, cambio.ZonaHoraria, cambio.Direccion, textoEstado(cambio.Estado)))
			if err != nil {
				return id.String(), err
			}
			return sede.Id.String(), nil
		})
	if err != nil {
		return api.Sede{}, err
	}

	return sede, nil
}

// --------------------------------------------------------------- servicios --

// ServiciosTodos devuelve los servicios del tenant, activos e inactivos.
func (s *Servicio) ServiciosTodos(ctx context.Context, tenant uuid.UUID) ([]api.Servicio, error) {
	servicios := make([]api.Servicio, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT s.id::text, s.sede_id::text, s.nombre, s.descripcion,
			       s.duracion_min, s.precio_monto::text, t.moneda, s.estado::text
			FROM negocio.servicio s
			JOIN plataforma.tenant t ON t.id = s.tenant_id
			ORDER BY s.nombre`)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			servicio, err := escanearServicio(filas)
			if err != nil {
				return err
			}
			servicios = append(servicios, servicio)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return servicios, nil
}

// CrearServicio da de alta un servicio (RF-30, RF-31).
//
// La moneda no se recibe ni se guarda aquí: vive en plataforma.tenant (RF-38) y
// se une al leer. Una copia por servicio permitiría que dos servicios del mismo
// negocio cobraran en monedas distintas, que no es una funcionalidad sino una
// forma de que un total no se pueda sumar.
func (s *Servicio) CrearServicio(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nuevo api.NuevoServicio,
) (api.Servicio, error) {
	var servicio api.Servicio

	_, err := s.enTx(ctx, tenant, actor, "crear_servicio", auditoria.RecursoServicio,
		func(tx pgx.Tx) (string, error) {
			var err error
			servicio, err = escanearServicio(tx.QueryRow(ctx, `
				WITH nuevo AS (
					INSERT INTO negocio.servicio
						(tenant_id, sede_id, nombre, descripcion, duracion_min, precio_monto)
					VALUES ($1, $2, $3, $4, $5, $6::numeric)
					RETURNING *
				)
				SELECT n.id::text, n.sede_id::text, n.nombre, n.descripcion,
				       n.duracion_min, n.precio_monto::text, t.moneda, n.estado::text
				FROM nuevo n JOIN plataforma.tenant t ON t.id = n.tenant_id`,
				tenant, nuevo.SedeId, nuevo.Nombre, nuevo.Descripcion,
				nuevo.DuracionMin, nuevo.PrecioMonto))
			if err != nil {
				return "", err
			}
			return servicio.Id.String(), nil
		})
	if err != nil {
		return api.Servicio{}, err
	}

	return servicio, nil
}

// ActualizarServicio edita un servicio (RF-30, RF-31).
//
// Cambiar el precio o la duración NO altera ninguna reserva existente: la
// reserva congela ambos al crearse. Es lo que permite editar el catálogo sin
// tener que revisar la agenda.
func (s *Servicio) ActualizarServicio(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor,
	id uuid.UUID, cambio api.ActualizacionServicio,
) (api.Servicio, error) {
	var servicio api.Servicio

	_, err := s.enTx(ctx, tenant, actor, "actualizar_servicio", auditoria.RecursoServicio,
		func(tx pgx.Tx) (string, error) {
			var err error
			servicio, err = escanearServicio(tx.QueryRow(ctx, `
				WITH editado AS (
					UPDATE negocio.servicio
					SET nombre       = COALESCE($2::text, nombre),
					    descripcion  = COALESCE($3::text, descripcion),
					    duracion_min = COALESCE($4::int, duracion_min),
					    precio_monto = COALESCE($5::numeric, precio_monto),
					    estado       = COALESCE($6::negocio.estado_catalogo, estado)
					WHERE id = $1
					RETURNING *
				)
				SELECT e.id::text, e.sede_id::text, e.nombre, e.descripcion,
				       e.duracion_min, e.precio_monto::text, t.moneda, e.estado::text
				FROM editado e JOIN plataforma.tenant t ON t.id = e.tenant_id`,
				id, cambio.Nombre, cambio.Descripcion, cambio.DuracionMin,
				cambio.PrecioMonto, textoEstado(cambio.Estado)))
			if err != nil {
				return id.String(), err
			}
			return servicio.Id.String(), nil
		})
	if err != nil {
		return api.Servicio{}, err
	}

	return servicio, nil
}

// ---------------------------------------------------------------- recursos --

// Recursos devuelve los recursos del tenant con los servicios que prestan.
func (s *Servicio) Recursos(ctx context.Context, tenant uuid.UUID) ([]api.Recurso, error) {
	recursos := make([]api.Recurso, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		// El array agregado en SQL y no una segunda consulta por recurso: con
		// N recursos, aquello serían N+1 viajes a través de PgBouncer para un
		// dato que el motor sabe juntar de una vez.
		filas, err := tx.Query(ctx, `
			SELECT r.id::text, r.sede_id::text, r.nombre, r.estado::text,
			       COALESCE(
			         (SELECT array_agg(sr.servicio_id::text ORDER BY sr.servicio_id)
			          FROM negocio.servicio_recurso sr
			          WHERE sr.tenant_id = r.tenant_id AND sr.recurso_id = r.id),
			         ARRAY[]::text[])
			FROM negocio.recurso r
			ORDER BY r.nombre`)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			recurso, err := escanearRecurso(filas)
			if err != nil {
				return err
			}
			recursos = append(recursos, recurso)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return recursos, nil
}

// CrearRecurso da de alta un recurso y declara qué servicios presta (RF-30).
//
// Las dos cosas en una transacción: un recurso sin servicios declarados existe
// pero no es reservable —el núcleo comprueba el par servicio/recurso antes de
// insertar— así que dejar el enlace para una segunda petición produciría un
// recurso a medio crear cada vez que la segunda no llegue.
func (s *Servicio) CrearRecurso(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nuevo api.NuevoRecurso,
) (api.Recurso, error) {
	var recurso api.Recurso

	_, err := s.enTx(ctx, tenant, actor, "crear_recurso", auditoria.RecursoRecurso,
		func(tx pgx.Tx) (string, error) {
			var id string
			if err := tx.QueryRow(ctx, `
				INSERT INTO negocio.recurso (tenant_id, sede_id, nombre)
				VALUES ($1, $2, $3)
				RETURNING id::text`,
				tenant, nuevo.SedeId, nuevo.Nombre).Scan(&id); err != nil {
				return "", err
			}

			if err := enlazarServicios(ctx, tx, tenant, id, nuevo.Servicios); err != nil {
				return id, err
			}

			var err error
			recurso, err = leerRecurso(ctx, tx, id)
			return id, err
		})
	if err != nil {
		return api.Recurso{}, err
	}

	return recurso, nil
}

// ActualizarRecurso edita un recurso (RF-30).
//
// La lista de servicios se REEMPLAZA cuando viaja, no se amplía: quitar un
// servicio de un recurso tiene que ser expresable, y con una operación que solo
// añade no lo es.
func (s *Servicio) ActualizarRecurso(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor,
	id uuid.UUID, cambio api.ActualizacionRecurso,
) (api.Recurso, error) {
	var recurso api.Recurso

	_, err := s.enTx(ctx, tenant, actor, "actualizar_recurso", auditoria.RecursoRecurso,
		func(tx pgx.Tx) (string, error) {
			etiqueta, err := tx.Exec(ctx, `
				UPDATE negocio.recurso
				SET nombre = COALESCE($2::text, nombre),
				    estado = COALESCE($3::negocio.estado_catalogo, estado)
				WHERE id = $1`,
				id, cambio.Nombre, textoEstado(cambio.Estado))
			if err != nil {
				return id.String(), err
			}
			if etiqueta.RowsAffected() == 0 {
				return id.String(), datos.ErrNoEncontrado
			}

			if cambio.Servicios != nil {
				if _, err := tx.Exec(ctx,
					"DELETE FROM negocio.servicio_recurso WHERE recurso_id = $1", id); err != nil {
					return id.String(), err
				}
				if err := enlazarServicios(ctx, tx, tenant, id.String(), cambio.Servicios); err != nil {
					return id.String(), err
				}
			}

			recurso, err = leerRecurso(ctx, tx, id.String())
			return id.String(), err
		})
	if err != nil {
		return api.Recurso{}, err
	}

	return recurso, nil
}

func enlazarServicios(
	ctx context.Context, tx pgx.Tx, tenant uuid.UUID, recursoID string, servicios *[]uuid.UUID,
) error {
	if servicios == nil {
		return nil
	}

	for _, servicioID := range *servicios {
		// La clave compuesta (tenant_id, servicio_id) de la FK es lo que impide
		// enlazar el servicio de otro tenant: no hace falta comprobarlo aquí,
		// el motor devuelve una violación de referencia.
		if _, err := tx.Exec(ctx, `
			INSERT INTO negocio.servicio_recurso (tenant_id, servicio_id, recurso_id)
			VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, tenant, servicioID, recursoID); err != nil {
			return err
		}
	}

	return nil
}

func leerRecurso(ctx context.Context, tx pgx.Tx, id string) (api.Recurso, error) {
	return escanearRecurso(tx.QueryRow(ctx, `
		SELECT r.id::text, r.sede_id::text, r.nombre, r.estado::text,
		       COALESCE(
		         (SELECT array_agg(sr.servicio_id::text ORDER BY sr.servicio_id)
		          FROM negocio.servicio_recurso sr
		          WHERE sr.tenant_id = r.tenant_id AND sr.recurso_id = r.id),
		         ARRAY[]::text[])
		FROM negocio.recurso r
		WHERE r.id = $1`, id))
}

// -------------------------------------------------------------- escaneres --

func escanearSede(fila pgx.Row) (api.Sede, error) {
	var (
		sede   api.Sede
		id     string
		estado string
	)

	if err := fila.Scan(&id, &sede.Nombre, &sede.ZonaHoraria, &sede.Direccion, &estado); err != nil {
		return api.Sede{}, err
	}

	var err error
	if sede.Id, err = uuid.Parse(id); err != nil {
		return api.Sede{}, err
	}
	sede.Estado = api.EstadoCatalogo(estado)

	return sede, nil
}

func escanearServicio(fila pgx.Row) (api.Servicio, error) {
	var (
		servicio   api.Servicio
		id, sedeID string
		estado     string
	)

	if err := fila.Scan(
		&id, &sedeID, &servicio.Nombre, &servicio.Descripcion,
		&servicio.DuracionMin, &servicio.Precio.Monto, &servicio.Precio.Moneda, &estado,
	); err != nil {
		return api.Servicio{}, err
	}

	var err error
	if servicio.Id, err = uuid.Parse(id); err != nil {
		return api.Servicio{}, err
	}
	if servicio.SedeId, err = uuid.Parse(sedeID); err != nil {
		return api.Servicio{}, err
	}
	servicio.Estado = api.EstadoCatalogo(estado)

	return servicio, nil
}

func escanearRecurso(fila pgx.Row) (api.Recurso, error) {
	var (
		recurso    api.Recurso
		id, sedeID string
		estado     string
		servicios  []string
	)

	if err := fila.Scan(&id, &sedeID, &recurso.Nombre, &estado, &servicios); err != nil {
		return api.Recurso{}, err
	}

	var err error
	if recurso.Id, err = uuid.Parse(id); err != nil {
		return api.Recurso{}, err
	}
	if recurso.SedeId, err = uuid.Parse(sedeID); err != nil {
		return api.Recurso{}, err
	}
	recurso.Estado = api.EstadoCatalogo(estado)

	lista := make([]uuid.UUID, 0, len(servicios))
	for _, texto := range servicios {
		servicioID, err := uuid.Parse(texto)
		if err != nil {
			return api.Recurso{}, err
		}
		lista = append(lista, servicioID)
	}
	recurso.Servicios = &lista

	return recurso, nil
}

// textoEstado convierte el enum del contrato en el texto que espera el motor,
// conservando el nulo.
//
// Hace falta porque COALESCE($n, estado) necesita saber si el campo viajó, y un
// *api.EstadoCatalogo no se puede pasar directamente a un parámetro que se
// castea a un enum de PostgreSQL.
func textoEstado(estado *api.EstadoCatalogo) *string {
	if estado == nil {
		return nil
	}
	texto := string(*estado)
	return &texto
}
