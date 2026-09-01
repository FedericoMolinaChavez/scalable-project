package rutas

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Verificador comprueba un token de acceso. Lo cumple identidad.Firmante.
//
// Es una interfaz y no el tipo concreto para que los servicios que solo
// VERIFICAN no tengan que construir un emisor de códigos, con su conexión SMTP
// y sus límites de envío, para hacer algo que no van a hacer nunca.
type Verificador interface {
	Verificar(token string, ahora time.Time) (identidad.Acceso, error)
}

// exigirAcceso deja pasar solo a quien presenta un token válido, y deja en el
// contexto lo que ese token demuestra.
//
// Se aplica por ruta y no a todo el servicio: el catálogo y la disponibilidad
// son públicos a propósito —mirar horarios libres no exige identificarse— y
// crear una reserva también, porque RF-01 admite reservar como invitado. Es
// justo eso lo que crea la necesidad de RF-02.
func exigirAcceso(verificador Verificador) transporte.Medio {
	return func(siguiente http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, hay := tokenDeCabecera(r)
			if !hay {
				transporte.Escribir(w, transporte.NoAutorizado.Cuerpo(r.Context(),
					"esta operación necesita el token que devuelve /v1/sesiones/token"))
				return
			}

			acceso, err := verificador.Verificar(token, time.Now())
			switch {
			case errors.Is(err, identidad.ErrTokenVencido):
				// El vencimiento SÍ se distingue, y no contradice lo de no dar
				// pistas: caducar no es un fallo de quien lo presenta, es el
				// funcionamiento normal. La interfaz necesita poder decir
				// "vuelve a identificarte" en vez de "no eres tú".
				transporte.Escribir(w, transporte.TokenVencido.Cuerpo(r.Context(),
					"el token caducó; pide otro código para volver a entrar"))
				return
			case err != nil:
				// Todo lo demás responde igual: mal formado, firma que no
				// cuadra, versión desconocida. Distinguirlos le diría a quien
				// está probando tokens en qué se está equivocando.
				transporte.Escribir(w, transporte.NoAutorizado.Cuerpo(r.Context(), ""))
				return
			}

			siguiente.ServeHTTP(w, r.WithContext(identidad.ConAcceso(r.Context(), acceso)))
		})
	}
}

// tokenDeCabecera extrae el token de `Authorization: Bearer <token>`.
//
// El esquema se compara sin distinguir mayúsculas porque RFC 9110 dice que no
// las distingue, y hay clientes que mandan "bearer".
func tokenDeCabecera(r *http.Request) (string, bool) {
	cabecera := r.Header.Get("Authorization")
	if cabecera == "" {
		return "", false
	}

	esquema, valor, hay := strings.Cut(cabecera, " ")
	if !hay || !strings.EqualFold(esquema, "Bearer") {
		return "", false
	}

	valor = strings.TrimSpace(valor)
	return valor, valor != ""
}
