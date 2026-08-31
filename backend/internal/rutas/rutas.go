// Package rutas monta la capa HTTP sobre los componentes de ARQ-01.
//
// Es el único sitio del backend que conoce a la vez los códigos de estado y los
// errores del dominio, y esa concentración es deliberada: los componentes
// devuelven errores con significado —"el horario ya está reservado"— y aquí se
// decide que eso es un 409. Con la decisión repartida, el mismo error acabaría
// siendo un 422 en una ruta y un 500 en otra.
//
// Un binario monta solo los componentes que le tocan. Los que no le tocan
// llegan en nil y sus rutas no se registran: pedirlas a este proceso devuelve
// el 404 del mux, que es exactamente lo que un Gateway mal configurado debería
// ver, en vez de un manejador vacío que finge existir.
package rutas

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Componentes son los servicios de ARQ-01 que este proceso sirve. Un nil
// significa "este binario no es dueño de esas rutas".
type Componentes struct {
	Catalogo       *catalogo.Servicio
	Disponibilidad *disponibilidad.Servicio
	Nucleo         *nucleo.Servicio
	Consulta       *consulta.Servicio
}

// Montar registra en el servidor las rutas de los componentes presentes.
func Montar(s *plataforma.Servidor, c Componentes, registro *slog.Logger, plazo time.Duration) {
	adaptador := &adaptador{c: c, registro: registro}

	// El manejador estricto obliga a que cada operación devuelva uno de los
	// tipos que el contrato declara para ella. Es lo que compra el generador:
	// un 404 en una ruta que no lo declara no compila.
	estricto := api.NewStrictHandlerWithOptions(adaptador, nil, api.StrictHTTPServerOptions{
		// Falla al decodificar el cuerpo: JSON roto o un campo con el tipo
		// equivocado. Es del cliente.
		RequestErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			transporte.Escribir(w, invalida(r.Context(), "el cuerpo de la petición no se pudo interpretar"))
		},

		// Falla al escribir la respuesta ya elegida. Es nuestro, y el detalle
		// no sale de aquí.
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			registro.ErrorContext(r.Context(), "no se pudo escribir la respuesta",
				slog.String("error", err.Error()),
				slog.String("peticion", transporte.IdPeticion(r.Context())))
			transporte.Escribir(w, transporte.Interno.Cuerpo(r.Context(), ""))
		},
	})

	// La envoltura enlaza parámetros de ruta, consulta y cabecera antes de
	// llegar al manejador. Su error por defecto es un http.Error en texto
	// plano, que rompería la promesa del formato único.
	envoltura := &api.ServerInterfaceWrapper{
		Handler: estricto,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			transporte.Escribir(w, invalida(r.Context(), err.Error()))
		},
	}

	medios := []transporte.Medio{
		// El orden importa: el identificador primero, porque todo lo demás lo
		// necesita para poder correlacionarse; la recuperación por fuera del
		// resto, para que un pánico dentro de las métricas también se atrape.
		transporte.ConIdPeticion(),
		transporte.ConRecuperacion(registro),
		transporte.ConRegistro(registro),
		transporte.ConMetricas(s.Metricas().PeticionesHTTP, s.Metricas().DuracionHTTP),
		transporte.ConTiempoLimite(plazo),
	}

	registrar := func(patron string, manejador http.HandlerFunc) {
		s.Registrar(patron, transporte.Encadenar(manejador, medios...))
	}

	if c.Catalogo != nil {
		registrar("GET /v1/sedes", envoltura.ListarSedes)
		registrar("GET /v1/servicios", envoltura.ListarServicios)
	}
	if c.Disponibilidad != nil {
		registrar("GET /v1/disponibilidad", envoltura.ConsultarDisponibilidad)
	}
	if c.Consulta != nil {
		registrar("GET /v1/reservas", envoltura.ListarReservas)
		registrar("GET /v1/reservas/{id}", envoltura.ObtenerReserva)
	}
	if c.Nucleo != nil {
		registrar("POST /v1/reservas", envoltura.CrearReserva)
	}
}
