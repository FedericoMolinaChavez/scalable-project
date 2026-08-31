package transporte

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
)

// Medio es un envoltorio de http.Handler. Se aplican por ruta y no sobre el
// mux entero: dentro de un manejador ya registrado, el mux ha resuelto la
// coincidencia y r.Pattern ya vale algo, que es la etiqueta que necesitan las
// métricas. Envolviendo el mux por fuera, el patrón todavía no existe y la
// única etiqueta disponible sería la URL cruda —con los UUID dentro—, que
// multiplica las series de Prometheus por cada reserva del sistema.
type Medio func(http.Handler) http.Handler

// Encadenar aplica los medios en el orden en que se escriben: el primero es el
// más externo, y por tanto el que ve la petición antes y la respuesta después.
func Encadenar(h http.Handler, medios ...Medio) http.Handler {
	for i := len(medios) - 1; i >= 0; i-- {
		h = medios[i](h)
	}
	return h
}

type claveContexto int

const claveIdPeticion claveContexto = iota

// CabeceraIdPeticion es la cabecera que transporta el identificador, de entrada
// y de salida.
const CabeceraIdPeticion = "X-Request-Id"

// IdPeticion devuelve el identificador de la petición en curso, o cadena vacía
// fuera de una.
func IdPeticion(ctx context.Context) string {
	id, _ := ctx.Value(claveIdPeticion).(string)
	return id
}

// ConIdPeticion asigna un identificador a cada petición y lo devuelve en la
// respuesta.
//
// Se respeta el que venga del cliente cuando existe, porque es lo que permite
// seguir una operación que atraviesa varios componentes de ARQ-01: si cada
// servicio generase el suyo, la misma reserva tendría un identificador
// distinto en el núcleo, en el de consulta y en el Gateway, y cruzar los
// registros dejaría de ser posible.
func ConIdPeticion() Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(CabeceraIdPeticion)
			if id == "" {
				id = uuid.NewString()
			}

			w.Header().Set(CabeceraIdPeticion, id)
			siguiente.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), claveIdPeticion, id)))
		})
	}
}

// ConRecuperacion evita que un pánico en un manejador tumbe el proceso entero.
//
// Sin esto, net/http ya recupera el pánico de una conexión, pero cierra el
// socket sin respuesta: el cliente ve un fallo de red y no un error, y no hay
// forma de correlacionarlo. Aquí se responde en el mismo formato que cualquier
// otro error, con el identificador de la petición dentro.
func ConRecuperacion(registro *slog.Logger) Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// contextcheck marca este defer porque escribe la respuesta sin
			// recibir un contexto por parámetro. Aquí no hay nada que
			// propagar: se está saliendo de un pánico, y el contexto que hace
			// falta —el de la petición, con su identificador— se lee de r,
			// que sigue siendo válido.
			//nolint:contextcheck // el contexto sale de r, ver arriba
			defer func() {
				panico := recover()
				if panico == nil {
					return
				}

				// http.ErrAbortHandler es la forma documentada de abortar una
				// respuesta a propósito. Tragarla convertiría un aborto
				// deliberado en un 500 registrado como fallo.
				if panico == http.ErrAbortHandler { //nolint:errorlint // valor centinela, no error envuelto
					panic(panico)
				}

				registro.ErrorContext(r.Context(), "pánico en el manejador",
					slog.Any("panico", panico),
					slog.String("ruta", r.Pattern),
					slog.String("peticion", IdPeticion(r.Context())))

				// Sin detalle: el mensaje de un pánico suele llevar dentro
				// fragmentos de estado interno, y esto va hacia el cliente.
				Escribir(w, Interno.Cuerpo(r.Context(), ""))
			}()

			siguiente.ServeHTTP(w, r)
		})
	}
}

// ConTiempoLimite acota cuánto puede durar una petición.
//
// No lo hacen los plazos del http.Server: aquellos cortan el socket, pero el
// manejador sigue vivo y su consulta sigue ocupando una conexión del pool.
// Cancelando el contexto, pgx aborta la consulta y la conexión vuelve al
// pooler. Es lo que impide que un pico de peticiones lentas agote las
// conexiones de PgBouncer y tumbe también a las rápidas.
func ConTiempoLimite(plazo time.Duration) Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancelar := context.WithTimeout(r.Context(), plazo)
			defer cancelar()

			siguiente.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ConMetricas cuenta y cronometra cada petición, etiquetando por patrón de
// ruta y no por URL.
func ConMetricas(peticiones *prometheus.CounterVec, duracion *prometheus.HistogramVec) Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inicio := time.Now()
			espia := &espiaEstado{ResponseWriter: w, estado: http.StatusOK}

			siguiente.ServeHTTP(espia, r)

			ruta := r.Pattern
			if ruta == "" {
				ruta = "desconocida"
			}

			duracion.WithLabelValues(r.Method, ruta).Observe(time.Since(inicio).Seconds())
			peticiones.WithLabelValues(r.Method, ruta, strconv.Itoa(espia.estado)).Inc()
		})
	}
}

// ConRegistro deja una línea por petición atendida.
func ConRegistro(registro *slog.Logger) Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			inicio := time.Now()
			espia := &espiaEstado{ResponseWriter: w, estado: http.StatusOK}

			siguiente.ServeHTTP(espia, r)

			// Nivel según el resultado: un 4xx es el cliente equivocándose y no
			// merece despertar a nadie; un 5xx sí. Registrarlo todo a `info`
			// obliga a filtrar por código en Loki para encontrar lo que falla.
			nivel := slog.LevelInfo
			if espia.estado >= http.StatusInternalServerError {
				nivel = slog.LevelError
			} else if espia.estado >= http.StatusBadRequest {
				nivel = slog.LevelWarn
			}

			registro.Log(r.Context(), nivel, "petición atendida",
				slog.String("metodo", r.Method),
				slog.String("ruta", r.Pattern),
				slog.Int("estado", espia.estado),
				slog.Duration("duracion", time.Since(inicio)),
				slog.String("peticion", IdPeticion(r.Context())))
		})
	}
}

// espiaEstado retiene el código de estado, que el ResponseWriter no expone una
// vez escrito.
type espiaEstado struct {
	http.ResponseWriter
	estado    int
	yaEscrito bool
}

func (e *espiaEstado) WriteHeader(estado int) {
	if e.yaEscrito {
		return
	}
	e.estado = estado
	e.yaEscrito = true
	e.ResponseWriter.WriteHeader(estado)
}

func (e *espiaEstado) Write(b []byte) (int, error) {
	// Escribir sin WriteHeader implica un 200 y deja `estado` en su valor
	// inicial, que ya es 200. Solo hace falta cerrar la puerta para que un
	// WriteHeader posterior no lo cambie.
	e.yaEscrito = true
	return e.ResponseWriter.Write(b)
}

func escribirJSON(w io.Writer, cuerpo any) error {
	return json.NewEncoder(w).Encode(cuerpo)
}
