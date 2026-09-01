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
type Filtro struct {
	Estados []api.EstadoReserva
	Desde   *time.Time
	Hasta   *time.Time
	Limite  int
	Cursor  string
}

// Alcance es de quién son las reservas que se piden.
//
// Hoy solo existe una forma de acotarlo —el correo que un token de invitado
// acredita (RF-02)— pero es un tipo y no una cadena suelta porque RF-23 dice
// que el alcance sale del tipo de cuenta: con RF-12 esto gana un CuentaID y un
// caso de administrador que ve el tenant entero (RF-32). Que sea un tipo hace
// que añadirlos sea un campo más, no una firma nueva en cada capa.
type Alcance struct {
	// Destino es el correo verificado. Vacío significa sin acotar, y eso NO es
	// un valor válido que este paquete acepte: lo rechaza, porque una consulta
	// de reservas sin alcance devuelve las de todo el mundo.
	Destino string
}

// ErrSinAlcance se devuelve cuando se pide una lista sin decir de quién.
//
// Falla cerrado a propósito. El error posible es no devolver nada; el
// inaceptable sería devolver las reservas de otra persona porque alguien se
// olvidó de pasar el alcance.
var ErrSinAlcance = errors.New("no se puede listar reservas sin saber de quién son")

// Listar devuelve una página de reservas, de la más reciente a la más antigua.
//
// El ALCANCE de esta ruta no es fijo: sale del tipo de cuenta que la llama
// (RF-23). Un `usuario` ve las suyas (RF-02), un `administrador` ve las de su
// tenant (RF-32), y un `agente` ve las de la cuenta a la que representa. Es una
// sola ruta y no tres porque RF-23 modela el alcance como la intersección entre
// la acción pedida y el alcance de la cuenta, no como rutas distintas por rol.
//
// SIN AUTENTICACIÓN no hay de dónde derivar ese alcance, así que devuelve las
// del tenant entero: en la práctica, el alcance del administrador. Eso NO
// convierte esta función en provisional —el filtro por tenant es correcto para
// ese rol— pero sí significa que hoy cualquiera ve lo que solo el
// administrador debería ver.
//
// Con RF-12 el alcance entra por parámetro y esta consulta gana un
// `cuenta_id = $n` cuando quien llama es un usuario. La lista del usuario
// además debe pasar por plataforma.indice_reserva_global en vez de filtrar
// aquí, para no abanicar las 64 particiones (RNF-02).
func (s *Servicio) Listar(
	ctx context.Context, tenant uuid.UUID, alcance Alcance, f Filtro,
) (api.ListaReservas, error) {
	if alcance.Destino == "" {
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
			WHERE cuenta_id IS NULL
			  AND lower(contacto_email) = $7
			  AND ($1::text[] IS NULL OR estado::text = ANY ($1::text[]))
			  AND ($2::timestamptz IS NULL OR lower(periodo) >= $2::timestamptz)
			  AND ($3::timestamptz IS NULL OR lower(periodo) <  $3::timestamptz)
			  AND ($4::timestamptz IS NULL
			       OR (creada_en, id) < ($4::timestamptz, $5::uuid))
			ORDER BY creada_en DESC, id DESC
			LIMIT $6`,
			estados, f.Desde, f.Hasta, desdeCursor, idCursor, limite+1, alcance.Destino)
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
	if alcance.Destino == "" {
		return api.Reserva{}, ErrSinAlcance
	}

	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		var err error
		reserva, err = reservas.Escanear(tx.QueryRow(ctx,
			`SELECT `+reservas.Columnas+`
			 FROM negocio.reserva
			 WHERE id = $1
			   AND cuenta_id IS NULL
			   AND lower(contacto_email) = $2`, id, alcance.Destino))
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
