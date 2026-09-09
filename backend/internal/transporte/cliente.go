package transporte

import (
	"context"
	"net"
	"net/http"
	"strings"
)

// De dónde llega la petición: lo que RF-12 manda registrar al iniciar sesión y
// lo que RF-25 muestra al listar las sesiones activas.
//
// Viaja por el contexto y no como parámetro porque el manejador generado desde
// el contrato NO recibe el *http.Request: recibe el contexto y el objeto de la
// petición ya decodificado. La cabecera User-Agent y la dirección remota no
// están en ese objeto —no son parte del contrato— así que o se recogen aquí o
// no llegan.
//
// Es exactamente el mismo mecanismo que ConIdPeticion, y la consecuencia útil
// es la misma: un manejador no puede inventárselas ni leerlas de un sitio
// distinto según quién lo escriba.

const claveCliente claveContexto = 1

// Cliente es lo que se sabe de quién hizo la petición sin preguntarle.
type Cliente struct {
	// Dispositivo es el User-Agent, recortado. Sirve para reconocer la propia
	// sesión en una lista, no para identificar a nadie.
	Dispositivo string

	// IP es la dirección remota, sin el puerto. Se guarda en una columna inet,
	// así que tiene que ser una dirección y no un `host:puerto`.
	IP string
}

// maxDispositivo acota lo que se guarda del User-Agent.
//
// Un User-Agent es texto que escribe el cliente y puede tener el tamaño que
// quiera: sin tope, cada inicio de sesión es una escritura de tamaño arbitrario
// en el primario. 200 caracteres bastan para distinguir un navegador de un
// móvil, que es todo lo que RF-25 necesita mostrar.
const maxDispositivo = 200

// DeCliente devuelve lo que se sabe de quién hizo la petición en curso.
func DeCliente(ctx context.Context) Cliente {
	cliente, _ := ctx.Value(claveCliente).(Cliente)
	return cliente
}

// ConCliente recoge el dispositivo y la IP y los deja en el contexto.
func ConCliente() Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cliente := Cliente{
				Dispositivo: recortar(r.UserAgent(), maxDispositivo),
				IP:          direccionDe(r),
			}

			siguiente.ServeHTTP(w, r.WithContext(
				context.WithValue(r.Context(), claveCliente, cliente)))
		})
	}
}

// direccionDe extrae la IP del cliente.
//
// NO se lee X-Forwarded-For, y es deliberado: esa cabecera la escribe quien
// llama, así que confiar en ella permite a cualquiera decir que viene de la
// dirección que quiera —y con ello ensuciar la lista de sesiones de RF-25 y la
// auditoría de RNF-36 con datos que se inventó—. En ARQ-03 quien está delante
// es el Gateway, y la dirección real la debe inyectar él en una cabecera propia
// que el Gateway sobrescriba siempre. Hasta que exista esa cabecera, la
// dirección de la conexión es lo único que no miente.
func direccionDe(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// Sin puerto: se toma tal cual. Ocurre con algunos transportes de
		// prueba, y una dirección sin puerto sigue siendo una dirección.
		return r.RemoteAddr
	}
	return host
}

func recortar(s string, maximo int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maximo {
		return s
	}
	return s[:maximo]
}
