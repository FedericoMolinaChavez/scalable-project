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
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
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

	// Auditoria es la LECTURA de RF-36. La escritura no está aquí porque no es
	// un componente que se monte: es una función que cada operación llama
	// dentro de su propia transacción, y por eso vive en el componente que
	// escribe y no en el enrutado.
	Auditoria *auditoria.Consulta

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
		// De dónde llega la petición: lo que RF-12 registra al entrar y lo que
		// RF-25 muestra al listar sesiones. Va aquí y no en el manejador
		// porque el manejador generado no recibe el *http.Request.
		transporte.ConCliente(),
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
		// Públicas: mirar el catálogo de un negocio no exige identificarse.
		registrar("GET /v1/sedes", envoltura.ListarSedes)
		registrar("GET /v1/servicios", envoltura.ListarServicios)

		// Y la configuración, que es el mismo componente por el otro lado. Va
		// bajo /v1/config y toda acotada: aquí no se mira, se cambia lo que
		// hace posible reservar (RF-14, RF-15, RF-17, RF-30, RF-31).
		acotada("GET /v1/config/sedes", envoltura.ListarSedesConfig)
		acotada("POST /v1/config/sedes", envoltura.CrearSede)
		acotada("PATCH /v1/config/sedes/{id}", envoltura.ActualizarSede)

		acotada("GET /v1/config/servicios", envoltura.ListarServiciosConfig)
		acotada("POST /v1/config/servicios", envoltura.CrearServicio)
		acotada("PATCH /v1/config/servicios/{id}", envoltura.ActualizarServicio)

		acotada("GET /v1/config/recursos", envoltura.ListarRecursos)
		acotada("POST /v1/config/recursos", envoltura.CrearRecurso)
		acotada("PATCH /v1/config/recursos/{id}", envoltura.ActualizarRecurso)

		acotada("GET /v1/config/reglas", envoltura.ListarReglas)
		acotada("POST /v1/config/reglas", envoltura.CrearRegla)
		acotada("DELETE /v1/config/reglas/{id}", envoltura.EliminarRegla)

		acotada("GET /v1/config/excepciones", envoltura.ListarExcepciones)
		acotada("POST /v1/config/excepciones", envoltura.CrearExcepcion)
		acotada("DELETE /v1/config/excepciones/{id}", envoltura.EliminarExcepcion)

		// Sin PATCH ni DELETE: politica_version es append-only por disparador
		// (RF-15). Publicar condiciones nuevas es publicar una versión nueva.
		acotada("GET /v1/config/politicas", envoltura.ListarPoliticas)
		acotada("POST /v1/config/politicas", envoltura.PublicarPolitica)

		// Sin PATCH: RF-17 admite crear o eliminar y nada más.
		acotada("GET /v1/config/vouchers", envoltura.ListarVouchers)
		acotada("POST /v1/config/vouchers", envoltura.CrearVoucher)
		acotada("DELETE /v1/config/vouchers/{id}", envoltura.EliminarVoucher)

		acotada("GET /v1/config/tarifas", envoltura.ListarTarifas)
		acotada("POST /v1/config/tarifas", envoltura.CrearTarifa)
		acotada("DELETE /v1/config/tarifas/{id}", envoltura.EliminarTarifa)
	}
	if c.Auditoria != nil {
		acotada("GET /v1/auditoria", envoltura.ConsultarAuditoria)
	}
	if c.Disponibilidad != nil {
		registrar("GET /v1/disponibilidad", envoltura.ConsultarDisponibilidad)
	}
	if c.Identidad != nil {
		// Públicas a propósito: son la puerta de entrada, y exigir un token
		// para pedir un token sería circular. Lo mismo vale para el alta
		// (RF-24), la recuperación (RF-18) y los dos canjes por enlace: quien
		// llega a ellas es justamente quien todavía no puede entrar.
		registrar("POST /v1/sesiones/codigo", envoltura.SolicitarCodigo)
		registrar("POST /v1/sesiones/token", envoltura.CanjearCodigo)
		registrar("POST /v1/sesiones/contrasena", envoltura.IniciarSesion)
		registrar("POST /v1/sesiones/enlace", envoltura.SolicitarEnlace)
		registrar("POST /v1/sesiones/enlace/canje", envoltura.CanjearEnlace)
		registrar("POST /v1/sesiones/refresco", envoltura.RefrescarSesion)

		registrar("POST /v1/cuentas", envoltura.RegistrarCuenta)
		registrar("POST /v1/cuentas/verificacion/canje", envoltura.CanjearVerificacion)
		registrar("POST /v1/cuentas/contrasena/recuperacion", envoltura.SolicitarRecuperacion)
		registrar("POST /v1/cuentas/contrasena/restablecimiento", envoltura.RestablecerContrasena)

		// La credencial del agente ES la credencial de esta ruta (RF-13).
		registrar("POST /v1/agentes/token", envoltura.IntercambiarTokenAgente)

		// Y las que sí exigen cuenta. Un token de invitado llega hasta aquí
		// —es un token válido— y lo rechaza el manejador con un 403, no el
		// middleware: la diferencia entre "no te identificaste" y "lo que
		// traes no acredita una cuenta" es justo lo que la interfaz necesita
		// para saber si ofrecer iniciar sesión o registrarse.
		acotada("GET /v1/sesiones", envoltura.ListarSesiones)
		acotada("DELETE /v1/sesiones", envoltura.RevocarTodasLasSesiones)
		acotada("DELETE /v1/sesiones/{id}", envoltura.RevocarSesion)

		acotada("GET /v1/cuentas/yo", envoltura.ObtenerCuentaPropia)
		acotada("PATCH /v1/cuentas/yo", envoltura.ActualizarCuentaPropia)
		acotada("DELETE /v1/cuentas/yo", envoltura.EliminarCuentaPropia)
		acotada("GET /v1/cuentas/yo/preferencias", envoltura.ListarPreferencias)
		acotada("PUT /v1/cuentas/yo/preferencias", envoltura.GuardarPreferencias)
		acotada("POST /v1/cuentas/yo/contrasena", envoltura.CambiarContrasena)
		acotada("POST /v1/cuentas/verificacion", envoltura.SolicitarVerificacion)
	}
	if c.Consulta != nil {
		acotada("GET /v1/reservas", envoltura.ListarReservas)
		acotada("GET /v1/reservas/{id}", envoltura.ObtenerReserva)
	}
	if c.Nucleo != nil {
		// Crear es pública: RF-01 admite reservar como invitado, y es
		// justamente eso lo que hace falta que exista RF-02 después. Pero el
		// token se mira SI VIENE, porque la misma ruta la usa una cuenta
		// (RF-12) y un agente en su nombre (RF-04), y sin mirarlo esas dos
		// reservas quedarían como de invitado.
		if c.Verificador == nil {
			panic("rutas: POST /v1/reservas mira el token opcional y no se pasó un Verificador")
		}
		s.Registrar("POST /v1/reservas", transporte.Encadenar(
			http.HandlerFunc(envoltura.CrearReserva),
			append(append([]transporte.Medio{}, medios...), accesoOpcional(c.Verificador))...))
		acotada("POST /v1/reservas/{id}/cancelacion", envoltura.CancelarReserva)

		// Lo que se le hace a una reserva que ya existe. Las tres escriben en
		// negocio.reserva o dependen de su estado dentro de la transacción, así
		// que son del núcleo por la misma frontera que las dos de arriba.
		acotada("POST /v1/reservas/{id}/modificacion", envoltura.ModificarReserva)
		acotada("POST /v1/reservas/{id}/estado", envoltura.CambiarEstadoReserva)
		acotada("POST /v1/reservas/{id}/calificacion", envoltura.CalificarReserva)
	}
}
