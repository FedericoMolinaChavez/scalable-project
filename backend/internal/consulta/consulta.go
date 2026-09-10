// Package consulta es el componente "Consulta de Reservas" de ARQ-01: la ruta
// de lectura de RF-02, RF-03 y los filtros de RF-09.
//
// Solo lee. En despliegue va contra las réplicas y tolera el retraso de
// replicación que RNF-10 autoriza; en desarrollo hay un solo PostgreSQL y la
// distinción no se nota, que es precisamente por lo que el código no debe
// asumirla: nada aquí puede depender de leer lo que se acaba de escribir.
package consulta

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/reservas"
)

// Límites de página. El máximo lo fija el contrato; el valor por defecto
// también, y se repite aquí porque el generador no materializa los `default`
// del OpenAPI: llegan como puntero nulo y alguien tiene que decidir.
const (
	LimitePorDefecto = 20
	LimiteMaximo     = 100
)

// ErrCursorInvalido: el cursor no lo produjo este servicio, o llegó cortado.
//
// Se rechaza en vez de ignorarlo. Un cursor ilegible tratado como "empieza por
// el principio" devuelve al cliente a la primera página sin decírselo, y desde
// fuera eso se ve como una lista que se repite sola.
var ErrCursorInvalido = errors.New("el cursor de paginación no es válido")

// Servicio lee reservas.
type Servicio struct {
	bd *datos.BD
}

func Nuevo(bd *datos.BD) *Servicio {
	return &Servicio{bd: bd}
}

// Filtro son los criterios de RF-09, ya normalizados.
//
// Sede y recurso son los que RF-32 necesita para recorrer la agenda del
// administrador. No estan acotados a ese rol: filtrar lo propio por sede es una
// consulta legitima de cualquiera, y prohibirsela obligaria a que el filtro
// supiera de tipos de cuenta. Quien ve que lo decide el Alcance, no el filtro.
type Filtro struct {
	Estados []api.EstadoReserva
	Desde   *time.Time
	Hasta   *time.Time
	Sede    *uuid.UUID
	Recurso *uuid.UUID
	Limite  int
	Cursor  string
}

// Alcance es de quién son las reservas que se piden.
//
// Es un alias de dominio.Alcance y no un tipo propio: el mismo alcance gobierna
// la lectura de aquí y la cancelación del núcleo, y dos definiciones separadas
// derivarían en cuanto alguien añadiera un caso a una sola.
type Alcance = dominio.Alcance

// ErrSinAlcance es el de dominio, reexportado.
//
// Se conserva el nombre en este paquete porque internal/rutas lo clasifica por
// él y porque las pruebas de aquí lo esperan; lo que no se conserva es una
// segunda definición, que acabaría siendo un error distinto con el mismo texto.
var ErrSinAlcance = dominio.ErrSinAlcance

// Listar devuelve una página de reservas, de la más reciente a la más antigua.
//
// El ALCANCE no es fijo: sale del tipo de cuenta que la llama (RF-23). Un
// `usuario` ve las suyas (RF-02), un `administrador` ve las de su tenant
// (RF-32), un invitado ve las hechas con su correo, y un agente ve las de la
// cuenta a la que representa (RF-05). Es una sola ruta y no cuatro porque RF-23
// modela el alcance como la intersección entre la acción pedida y el alcance de
// la cuenta, no como rutas distintas por rol.
//
// Una cuenta ve además las reservas que hizo como INVITADO con su correo ya
// verificado. Es lo que RF-24 llama "asociar las reservas previas", resuelto en
// la lectura y no con una escritura al verificar: negocio.reserva está
// particionada por tenant y no hay índice global de reservas de invitado, así
// que reasignarlas todas exigiría barrer las 64 particiones de cada tenant sin
// saber en cuáles hay algo. Aquí la consulta ya está acotada al tenant que se
// está mirando, y lo que ve la persona es lo mismo.
func (s *Servicio) Listar(
	ctx context.Context, tenant uuid.UUID, alcance Alcance, f Filtro,
) (api.ListaReservas, error) {
	if alcance.Vacio() {
		return api.ListaReservas{}, ErrSinAlcance
	}

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
		c, err := descodificarCursor(f.Cursor)
		if err != nil {
			return api.ListaReservas{}, err
		}
		desdeCursor, idCursor = &c.creadaEn, &c.id
	}

	var estados []string
	for _, e := range f.Estados {
		estados = append(estados, string(e))
	}

	lista := api.ListaReservas{Datos: make([]api.Reserva, 0, limite)}

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		// Se piden limite+1 filas y se devuelven limite. La de más no se
		// muestra: existe solo para saber si hay página siguiente. La
		// alternativa —emitir cursor siempre que la página venga llena—
		// produce una última página vacía cada vez que el total es múltiplo
		// del límite, y el cliente no puede distinguirla de un fallo.
		// El alcance va en el WHERE, no en un filtro posterior sobre las filas
		// ya traídas. Filtrar después significaría que la base devolvió
		// reservas ajenas y que solo un `if` impidió enseñarlas; aquí nunca
		// salen de PostgreSQL.
		//
		// lower(contacto_email) usa el índice reserva_por_contacto, que está
		// definido sobre esa misma expresión. Escribirlo de otra forma
		// —comparando sin lower, o con ILIKE— haría que el índice no se use y
		// la consulta abanicara las 64 particiones.
		filas, err := tx.Query(ctx, `
			SELECT `+reservas.Columnas+`
			FROM negocio.reserva
			WHERE (
			        $7::boolean
			        OR ($8::uuid IS NOT NULL AND cuenta_id = $8::uuid)
			        OR ($9::text IS NOT NULL
			            AND cuenta_id IS NULL
			            AND lower(contacto_email) = $9::text)
			      )
			  AND ($1::text[] IS NULL OR estado::text = ANY ($1::text[]))
			  AND ($2::timestamptz IS NULL OR lower(periodo) >= $2::timestamptz)
			  AND ($3::timestamptz IS NULL OR lower(periodo) <  $3::timestamptz)
			  AND ($10::uuid IS NULL OR recurso_id = $10::uuid)
			  AND ($11::uuid IS NULL OR recurso_id IN (
			        SELECT r.id FROM negocio.recurso r WHERE r.sede_id = $11::uuid))
			  AND ($4::timestamptz IS NULL
			       OR (creada_en, id) < ($4::timestamptz, $5::uuid))
			ORDER BY creada_en DESC, id DESC
			LIMIT $6`,
			estados, f.Desde, f.Hasta, desdeCursor, idCursor, limite+1,
			alcance.TenantCompleto, nulo(alcance.Cuenta), nulo(alcance.Destino),
			f.Recurso, f.Sede)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			reserva, err := reservas.Escanear(filas)
			if err != nil {
				return err
			}
			lista.Datos = append(lista.Datos, reserva)
		}

		return filas.Err()
	})
	if err != nil {
		return api.ListaReservas{}, err
	}

	if len(lista.Datos) > limite {
		ultima := lista.Datos[limite-1]
		lista.Datos = lista.Datos[:limite]

		siguiente := codificarCursor(cursor{creadaEn: ultima.CreadaEn, id: ultima.Id.String()})
		lista.SiguienteCursor = &siguiente
	}

	return lista, nil
}

// Obtener devuelve una reserva concreta (RF-03).
//
// Una reserva que existe pero es de otra persona sale como datos.ErrNoEncontrado,
// igual que una que no existe. No es imprecisión: distinguirlas confirmaría qué
// identificadores son reales, y con eso se puede sondear la base ajena de a un
// UUID por vez.
func (s *Servicio) Obtener(
	ctx context.Context, tenant uuid.UUID, alcance Alcance, id uuid.UUID,
) (api.Reserva, error) {
	if alcance.Vacio() {
		return api.Reserva{}, ErrSinAlcance
	}

	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var err error
		reserva, err = reservas.Escanear(tx.QueryRow(ctx,
			`SELECT `+reservas.Columnas+`
			 FROM negocio.reserva
			 WHERE id = $1
			   AND (
			         $2::boolean
			         OR ($3::uuid IS NOT NULL AND cuenta_id = $3::uuid)
			         OR ($4::text IS NOT NULL
			             AND cuenta_id IS NULL
			             AND lower(contacto_email) = $4::text)
			       )`,
			id, alcance.TenantCompleto, nulo(alcance.Cuenta), nulo(alcance.Destino)))
		return err
	})
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}

// cursor es la posición exacta de la última fila entregada.
//
// Lleva creada_en Y el id porque creada_en no es único: dos reservas creadas en
// el mismo microsegundo —que con ~3.300 inserciones por segundo (RNF-03) no es
// hipotético— harían que un cursor sobre la marca de tiempo sola saltase una de
// las dos o repitiese la otra. El par sí es único, porque el id lo es.
type cursor struct {
	creadaEn time.Time
	id       string
}

// El cursor es opaco para el cliente a propósito: base64 de un par que solo
// este servicio sabe interpretar. Documentarlo invitaría a construirlo a mano,
// y entonces cambiar el orden de la consulta rompería clientes.
func codificarCursor(c cursor) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(c.creadaEn.UTC().Format(time.RFC3339Nano) + "|" + c.id))
}

func descodificarCursor(texto string) (cursor, error) {
	crudo, err := base64.RawURLEncoding.DecodeString(texto)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}

	marca, id, hay := strings.Cut(string(crudo), "|")
	if !hay {
		return cursor{}, ErrCursorInvalido
	}

	creadaEn, err := time.Parse(time.RFC3339Nano, marca)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}
	if _, err := uuid.Parse(id); err != nil {
		return cursor{}, fmt.Errorf("%w: %w", ErrCursorInvalido, err)
	}

	return cursor{creadaEn: creadaEn, id: id}, nil
}

// nulo convierte una cadena vacia en NULL para el motor.
//
// Hace falta porque el WHERE del alcance decide por "este criterio aplica o no"
// y no por "el valor esta vacio": con la cadena vacia, comparar
// lower(contacto_email) con ella puede resultar cierto sobre una fila cuyo
// correo tambien lo este, y esa reserva pasaria a ser de cualquiera. Con NULL,
// la comparacion es desconocida y la rama entera se apaga.
func nulo(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
