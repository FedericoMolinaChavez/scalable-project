// Package catalogo es el componente "Configuración y Catálogo" de ARQ-01: qué
// ofrece un tenant y dónde (RF-26).
//
// Este archivo es la mitad de LECTURA, la que consume el cliente final (RF-26):
// devuelve solo lo activo, porque una sede inactiva no es una que se muestra en
// gris, es una que no debe poder elegirse. La mitad de escritura —RF-14, RF-15,
// RF-17, RF-30 y RF-31— está en configuracion.go, calendario.go y comercial.go,
// y devuelve todo, porque un administrador necesita ver lo que ha apagado.
package catalogo

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Servicio sirve el catálogo de un tenant.
type Servicio struct {
	bd *datos.BD
}

func Nuevo(bd *datos.BD) *Servicio {
	return &Servicio{bd: bd}
}

// Sedes devuelve las sedes activas del tenant, ordenadas por nombre.
//
// Solo las activas. La ruta la consume el cliente final, y una sede inactiva no
// es "una sede que se muestra en gris": es una que no debe poder elegirse. El
// día que exista la pantalla del administrador necesitará verlas todas, y eso
// será un parámetro nuevo en el contrato, no un cambio silencioso aquí.
func (s *Servicio) Sedes(ctx context.Context, tenant uuid.UUID) ([]api.Sede, error) {
	return s.leerSedes(ctx, tenant, true)
}

// Servicios devuelve los servicios activos del tenant, opcionalmente los de una
// sola sede.
//
// La moneda no está en negocio.servicio: sale de plataforma.tenant, que es
// donde vive (RF-38). Se une aquí en vez de dejar que el manejador la busque
// aparte, para que un servicio no pueda llegar al cliente con un precio sin
// moneda.
func (s *Servicio) Servicios(ctx context.Context, tenant uuid.UUID, sede *uuid.UUID) ([]api.Servicio, error) {
	servicios := make([]api.Servicio, 0)

	var sedeFiltro *string
	if sede != nil {
		texto := sede.String()
		sedeFiltro = &texto
	}

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT s.id::text, s.sede_id::text, s.nombre, s.descripcion,
			       s.duracion_min, s.precio_monto::text, t.moneda, s.estado::text
			FROM negocio.servicio s
			JOIN plataforma.tenant t ON t.id = s.tenant_id
			WHERE s.estado = 'activo'
			  AND ($1::uuid IS NULL OR s.sede_id = $1::uuid)
			ORDER BY s.nombre`, sedeFiltro)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var (
				id, sedeID string
				servicio   api.Servicio
				estado     string
			)
			if err := filas.Scan(
				&id, &sedeID, &servicio.Nombre, &servicio.Descripcion,
				&servicio.DuracionMin, &servicio.Precio.Monto, &servicio.Precio.Moneda, &estado,
			); err != nil {
				return err
			}

			if servicio.Id, err = uuid.Parse(id); err != nil {
				return err
			}
			if servicio.SedeId, err = uuid.Parse(sedeID); err != nil {
				return err
			}
			servicio.Estado = api.EstadoCatalogo(estado)

			servicios = append(servicios, servicio)
		}

		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return servicios, nil
}
