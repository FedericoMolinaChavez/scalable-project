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
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
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
	Identidad      *identidad.Servicio

	// Verificador comprueba los tokens de acceso. Es obligatorio en cuanto el
	// proceso monte alguna ruta acotada, y Montar se niega a arrancar sin él:
	// una ruta que exige identificación sin nadie que la compruebe no falla,
	// es peor, sirve los datos igual.
	Verificador Verificador
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

	// Las rutas acotadas llevan un medio más, el que exige el token. Va el
	// último de la cadena, ya con el identificador de petición asignado y
	// dentro de la recuperación de pánico: un rechazo también tiene que poder
	// buscarse en los registros.
	acotada := func(patron string, manejador http.HandlerFunc) {
		if c.Verificador == nil {
			panic("rutas: " + patron + " exige identificación y no se pasó un Verificador")
		}
		s.Registrar(patron, transporte.Encadenar(manejador,
			append(append([]transporte.Medio{}, medios...), exigirAcceso(c.Verificador))...))
	}

	if c.Catalogo != nil {
		registrar("GET /v1/sedes", envoltura.ListarSedes)
		registrar("GET /v1/servicios", envoltura.ListarServicios)
	}
	if c.Disponibilidad != nil {
		registrar("GET /v1/disponibilidad", envoltura.ConsultarDisponibilidad)
	}
	if c.Identidad != nil {
		// Públicas a propósito: son la puerta de entrada, y exigir un token
		// para pedir un token sería circular.
		registrar("POST /v1/sesiones/codigo", envoltura.SolicitarCodigo)
		registrar("POST /v1/sesiones/token", envoltura.CanjearCodigo)
	}
	if c.Consulta != nil {
		acotada("GET /v1/reservas", envoltura.ListarReservas)
		acotada("GET /v1/reservas/{id}", envoltura.ObtenerReserva)
	}
	if c.Nucleo != nil {
		// Crear es pública: RF-01 admite reservar como invitado, y es
		// justamente eso lo que hace falta que exista RF-02 después.
		registrar("POST /v1/reservas", envoltura.CrearReserva)
		acotada("POST /v1/reservas/{id}/cancelacion", envoltura.CancelarReserva)
	}
}
