package auditoria

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// La otra mitad de RF-36: consultar la traza.
//
// El alcance lo resuelve RLS y no un WHERE de aquí: la transacción corre con el
// tenant del administrador fijado, así que "solo su tenant" no es algo que este
// código tenga que acordarse de escribir. Un `super_admin` verá todos los
// tenants cuando exista su superficie, y lo hará con el rol que atraviesa el
// aislamiento (`reservas_soporte`), no con un filtro distinto aquí.

// Límites de página. El máximo lo fija el contrato; el valor por defecto
// también, y se repite aquí porque el generador no materializa los `default`.
const (
	LimitePorDefecto = 50
	LimiteMaximo     = 200
)

// ErrCursorInvalido: el cursor no lo produjo este servicio, o llegó cortado.
var ErrCursorInvalido = errors.New("el cursor de paginación no es válido")

// Consulta lee la auditoría.
//
// Es un tipo aparte del que escribe porque son dos caminos distintos: escribir
// ocurre dentro de la transacción de otra operación y por eso es una función
// suelta que recibe una pgx.Tx; leer es una consulta propia contra las
// réplicas, y necesita su pool.
type Consulta struct {
	bd *datos.BD
}

func NuevaConsulta(bd *datos.BD) *Consulta {
	return &Consulta{bd: bd}
}

// Filtro son los criterios de RF-36: actor, recurso, acción o rango de fechas.
type Filtro struct {
	Desde       *time.Time
	Hasta       *time.Time
	Actor       *uuid.UUID
	RecursoTipo string
	Accion      string
	Limite      int
	Cursor      string
}

// Listar devuelve una página de eventos, del más reciente al más antiguo.
func (c *Consulta) Listar(
	ctx context.Context, tenant uuid.UUID, f Filtro,
) (api.ListaAuditoria, error) {
	limite := f.Limite
	if limite <= 0 {
		limite = LimitePorDefecto
	}
	if limite > LimiteMaximo {
		limite = LimiteMaximo
	}

	var (
		desdeCursor *time.Time
		idCursor    *string
	)
	if f.Cursor != "" {
		marca, id, err := descodificarCursor(f.Cursor)
		if err != nil {
			return api.ListaAuditoria{}, err
		}
		desdeCursor, idCursor = &marca, &id
	}

	lista := api.ListaAuditoria{Datos: make([]api.EventoAuditoria, 0, limite)}

	err := c.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		// Se piden limite+1 filas y se devuelven limite, igual que en el
		// listado de reservas: la de más no se muestra, existe solo para saber
		// si hay página siguiente.
		filas, err := tx.Query(ctx, `
			SELECT id::text, ocurrido_en, actor_tipo::text, actor_id::text,
			       agente_id::text, cuenta_impersonada_id::text, alcance_token,
			       accion, recurso_tipo, recurso_id::text, resultado::text,
			       host(ip), dispositivo
			FROM negocio.evento_auditoria
			WHERE ($1::timestamptz IS NULL OR ocurrido_en >= $1::timestamptz)
			  AND ($2::timestamptz IS NULL OR ocurrido_en <  $2::timestamptz)
			  AND ($3::uuid IS NULL OR actor_id = $3::uuid)
			  AND ($4::text IS NULL OR recurso_tipo = $4::text)
			  AND ($5::text IS NULL OR accion = $5::text)
			  AND ($6::timestamptz IS NULL
			       OR (ocurrido_en, id) < ($6::timestamptz, $7::uuid))
			ORDER BY ocurrido_en DESC, id DESC
			LIMIT $8`,
			f.Desde, f.Hasta, f.Actor, nulo(f.RecursoTipo), nulo(f.Accion),
			desdeCursor, idCursor, limite+1)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			evento, err := escanear(filas)
			if err != nil {
				return err
			}
			lista.Datos = append(lista.Datos, evento)
		}
		return filas.Err()
	})
	if err != nil {
		return api.ListaAuditoria{}, err
	}

	if len(lista.Datos) > limite {
		ultimo := lista.Datos[limite-1]
		lista.Datos = lista.Datos[:limite]

		siguiente := codificarCursor(ultimo.OcurridoEn, ultimo.Id.String())
		lista.SiguienteCursor = &siguiente
	}

	return lista, nil
}

func escanear(fila pgx.Row) (api.EventoAuditoria, error) {
	var (
		evento               api.EventoAuditoria
		id                   string
		actorID, agenteID    *string
		impersonada          *string
		recursoID            *string
		alcance              []byte
		actorTipo, resultado string
	)

	if err := fila.Scan(&id, &evento.OcurridoEn, &actorTipo, &actorID,
		&agenteID, &impersonada, &alcance,
		&evento.Accion, &evento.RecursoTipo, &recursoID, &resultado,
		&evento.Ip, &evento.Dispositivo); err != nil {
		return api.EventoAuditoria{}, err
	}

	var err error
	if evento.Id, err = uuid.Parse(id); err != nil {
		return api.EventoAuditoria{}, err
	}
	evento.ActorTipo = api.EventoAuditoriaActorTipo(actorTipo)
	evento.Resultado = api.ResultadoAuditoria(resultado)

	if evento.ActorId, err = comoUUID(actorID); err != nil {
		return api.EventoAuditoria{}, err
	}
	if evento.AgenteId, err = comoUUID(agenteID); err != nil {
		return api.EventoAuditoria{}, err
	}
	if evento.CuentaImpersonadaId, err = comoUUID(impersonada); err != nil {
		return api.EventoAuditoria{}, err
	}
	if evento.RecursoId, err = comoUUID(recursoID); err != nil {
		return api.EventoAuditoria{}, err
	}

	if len(alcance) > 0 {
		var acciones []string
		if err := json.Unmarshal(alcance, &acciones); err != nil {
			return api.EventoAuditoria{}, fmt.Errorf("el alcance guardado no es una lista: %w", err)
		}
		evento.AlcanceToken = &acciones
	}

	return evento, nil
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

func nulo(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// El cursor lleva el instante Y el identificador porque ocurrido_en no es
// único: dos eventos del mismo microsegundo —que con una operación que audita
// varias filas no es hipotético— harían que un cursor sobre la marca de tiempo
// sola saltase uno de los dos o repitiese el otro.
func codificarCursor(marca time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(marca.UTC().Format(time.RFC3339Nano) + "|" + id))
}

func descodificarCursor(texto string) (time.Time, string, error) {
	crudo, err := base64.RawURLEncoding.DecodeString(texto)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}

	marca, id, hay := strings.Cut(string(crudo), "|")
	if !hay {
		return time.Time{}, "", ErrCursorInvalido
	}

	ocurrido, err := time.Parse(time.RFC3339Nano, marca)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}
	if _, err := uuid.Parse(id); err != nil {
		return time.Time{}, "", fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}

	return ocurrido, id, nil
}
