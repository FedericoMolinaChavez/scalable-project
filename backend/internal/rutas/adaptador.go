package rutas

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// adaptador implementa la interfaz que el generador produce desde el contrato.
//
// Cada método hace lo mismo: extraer los parámetros, llamar al componente y
// convertir el error en una de las respuestas que ESA operación declara. Lo
// repetitivo es el precio de que el compilador impida devolver un código que el
// contrato no permite, y es un buen precio: el fallo que evita —una ruta que
// responde algo que el cliente generado no sabe leer— solo aparecería en
// ejecución y contra un frontend ya desplegado.
//
// Cada operación mapea exactamente los códigos que su componente puede
// producir, ni uno más. Una rama para un 404 que ninguna consulta de catálogo
// devuelve nunca es código que nadie ejecuta y que aun así hay que leer y
// mantener.
type adaptador struct {
	c        Componentes
	registro *slog.Logger
}

var _ api.StrictServerInterface = (*adaptador)(nil)

// errSinComponente no puede ocurrir por una petición: Montar no registra la
// ruta cuando su componente falta. Solo se alcanza si alguien registra una ruta
// a mano y se olvida del componente, y entonces es un fallo de programación que
// merece verse como un 500 con su traza, no como una respuesta plausible.
var errSinComponente = errors.New("este proceso no sirve esa ruta")

// interno registra la causa real y devuelve el cuerpo que sí sale hacia fuera.
//
// Son dos cosas distintas a propósito. Dentro queda todo lo que hace falta para
// diagnosticar —el error envuelto, el SQLSTATE, la petición—; fuera va un
// problema sin detalle, porque el mensaje de un error de pgx lleva nombres de
// columnas y fragmentos de consulta. El identificador de la petición está en
// ambos lados, y es lo que permite pasar de un informe de usuario a la línea
// exacta del registro.
func (a *adaptador) interno(ctx context.Context, operacion string, err error) api.Problema {
	a.registro.ErrorContext(ctx, "la operación falló",
		slog.String("operacion", operacion),
		slog.String("error", err.Error()),
		slog.String("sqlstate", datos.CodigoPG(err)),
		slog.String("peticion", transporte.IdPeticion(ctx)))

	return transporte.Interno.Cuerpo(ctx, "")
}

// ---------------------------------------------------------------- catálogo --

func (a *adaptador) ListarSedes(
	ctx context.Context, pet api.ListarSedesRequestObject,
) (api.ListarSedesResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	sedes, err := a.c.Catalogo.Sedes(ctx, pet.Params.XTenantId)
	if err != nil {
		return api.ListarSedes500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarSedes", err),
			),
		}, nil
	}

	return api.ListarSedes200JSONResponse{Datos: sedes}, nil
}

func (a *adaptador) ListarServicios(
	ctx context.Context, pet api.ListarServiciosRequestObject,
) (api.ListarServiciosResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	servicios, err := a.c.Catalogo.Servicios(ctx, pet.Params.XTenantId, pet.Params.SedeId)
	if err != nil {
		return api.ListarServicios500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarServicios", err),
			),
		}, nil
	}

	return api.ListarServicios200JSONResponse{Datos: servicios}, nil
}

// ---------------------------------------------------------- disponibilidad --

func (a *adaptador) ConsultarDisponibilidad(
	ctx context.Context, pet api.ConsultarDisponibilidadRequestObject,
) (api.ConsultarDisponibilidadResponseObject, error) {
	if a.c.Disponibilidad == nil {
		return nil, errSinComponente
	}

	resultado, err := a.c.Disponibilidad.Consultar(
		ctx, pet.Params.XTenantId, pet.Params.ServicioId, pet.Params.Desde, pet.Params.Hasta)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusBadRequest:
			return api.ConsultarDisponibilidad400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusNotFound:
			return api.ConsultarDisponibilidad404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.ConsultarDisponibilidad500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "consultarDisponibilidad", err),
				),
			}, nil
		}
	}

	return api.ConsultarDisponibilidad200JSONResponse(resultado), nil
}

// -------------------------------------------------------- núcleo (escribe) --

func (a *adaptador) CrearReserva(
	ctx context.Context, pet api.CrearReservaRequestObject,
) (api.CrearReservaResponseObject, error) {
	if a.c.Nucleo == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.CrearReserva400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	// El token es OPCIONAL en esta ruta: RF-01 admite reservar como invitado.
	// Cuando viene, dice de quién es la reserva —y si la está haciendo un
	// agente en su nombre (RF-04)—, que es lo que decide si se escribe
	// cuenta_id y el índice global.
	acceso, _ := identidad.DeAcceso(ctx)
	if !acceso.Permite(string(api.Reservar)) {
		return api.CrearReserva422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				transporte.FueraDeAlcance.Cuerpo(ctx,
					"ese token de agente no incluye reservar"),
			),
		}, nil
	}

	reserva, err := a.c.Nucleo.Crear(ctx, nucleo.Peticion{
		Tenant:            tenantDe(ctx, pet.Params.XTenantId),
		ClaveIdempotencia: pet.Params.IdempotencyKey,
		Nueva:             *pet.Body,
		Cuenta:            acceso.Cuenta,
		Agente:            acceso.Agente,
	})
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusBadRequest:
			return api.CrearReserva400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusNotFound:
			return api.CrearReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			// El 409 lo declara la ruta en línea, sin respuesta compartida: es
			// el único código cuyo significado es propio de ESTA operación.
			return api.CrearReserva409ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusUnprocessableEntity:
			return api.CrearReserva422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.CrearReserva500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "crearReserva", err),
				),
			}, nil
		}
	}

	// pago_client_secret va ausente: el PaymentIntent de Stripe no entra en
	// esta rebanada. El campo es opcional en el contrato precisamente para que
	// su ausencia sea una respuesta válida, y no un hueco relleno con cadena
	// vacía que el cliente tomaría por un secreto de verdad.
	return api.CrearReserva201JSONResponse{Reserva: reserva}, nil
}

func (a *adaptador) CancelarReserva(
	ctx context.Context, pet api.CancelarReservaRequestObject,
) (api.CancelarReservaResponseObject, error) {
	if a.c.Nucleo == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.CancelarReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, ""),
			),
		}, nil
	}

	// Un agente solo cancela si su token lo dice (RF-13). El alcance ya vino
	// recortado al emitirlo, así que aquí no se decide nada nuevo: se comprueba
	// que esta petición concreta esté dentro de lo que se concedió.
	acceso, _ := identidad.DeAcceso(ctx)
	if !acceso.Permite(string(api.CancelarReserva)) {
		return api.CancelarReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx,
					"ese token de agente no incluye cancelar reservas"),
			),
		}, nil
	}

	reserva, err := a.c.Nucleo.Cancelar(
		ctx, tenantDe(ctx, pet.Params.XTenantId), pet.Id, alcance, acceso.Agente)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusNotFound:
			// Incluye la reserva que existe pero es de otra persona. Un 403
			// confirmaría que ese identificador es real.
			return api.CancelarReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			return api.CancelarReserva409ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusUnprocessableEntity:
			return api.CancelarReserva422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.CancelarReserva500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "cancelarReserva", err),
				),
			}, nil
		}
	}

	return api.CancelarReserva200JSONResponse(reserva), nil
}

// alcance saca del token de quién son las reservas que se piden (RF-23).
//
// Es la única traducción de "quién eres" a "qué puedes ver", y por eso vive en
// un solo sitio: repartirla por manejador haría que la misma persona tuviera un
// alcance en el listado y otro en el detalle, y ninguna prueba lo notaría hasta
// que alguien viera lo que no debía.
//
// Los tres casos salen del propio token:
//
//	administrador -> su tenant entero (RF-32). La transacción ya corre con su
//	                 tenant fijado, así que RLS acota el resto.
//	cuenta        -> lo suyo, más lo que hizo como invitado con su correo YA
//	                 verificado (RF-02 + RF-24).
//	invitado      -> las reservas hechas con el correo que el código acreditó.
//
// El segundo valor es falso cuando no hay acceso en el contexto, que solo puede
// ocurrir si la ruta se montó sin exigirAcceso. Falla cerrado —responde 401— en
// vez de continuar sin acotar: el error posible es negar el acceso a quien lo
// tenía; el inaceptable sería servirle a alguien las reservas de todo el mundo
// por un fallo de cableado.
func (a *adaptador) alcance(ctx context.Context) (consulta.Alcance, bool) {
	acceso, hay := identidad.DeAcceso(ctx)
	if !hay {
		a.registro.ErrorContext(ctx, "ruta acotada sin acceso en el contexto",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return consulta.Alcance{}, false
	}

	alcance := consulta.Alcance{
		Cuenta:  acceso.Cuenta,
		Destino: acceso.Destino,

		// Un administrador ve su tenant entero. Un agente NO hereda eso aunque
		// represente a un administrador: RF-13 concede acciones sobre UNA
		// cuenta, y "toda la agenda del negocio" no es una acción sobre una
		// cuenta.
		TenantCompleto: acceso.Tipo == identidad.TipoAdmin && !acceso.PorAgente(),
	}

	if alcance.Vacio() {
		a.registro.ErrorContext(ctx, "token verificado sin nada que acredite un alcance",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return consulta.Alcance{}, false
	}

	return alcance, true
}

// tenantDe decide sobre qué negocio opera la petición.
//
// Un administrador opera sobre el suyo, salga lo que salga en la cabecera: el
// tenant va DENTRO de su token y aceptar el de la cabecera permitiría leer los
// datos de cualquier otro negocio escribiendo su identificador. Para todos los
// demás sigue mandando la cabecera, que es lo que hoy identifica al negocio en
// una ruta pública (catálogo, disponibilidad, crear reserva).
func tenantDe(ctx context.Context, cabecera openapi_types.UUID) openapi_types.UUID {
	acceso, hay := identidad.DeAcceso(ctx)
	if !hay || acceso.Tenant == "" {
		return cabecera
	}

	propio, err := uuid.Parse(acceso.Tenant)
	if err != nil {
		// Un tenant ilegible dentro de un token firmado por nosotros es un
		// fallo nuestro, no de la petición. Se cae del lado seguro: la
		// cabecera, que ya está acotada por RLS.
		return cabecera
	}

	return propio
}

// ------------------------------------------------------------- identidad --

func (a *adaptador) SolicitarCodigo(
	ctx context.Context, pet api.SolicitarCodigoRequestObject,
) (api.SolicitarCodigoResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.SolicitarCodigo400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.Solicitar(ctx, string(pet.Body.Destino))

	switch {
	case err == nil:
		return api.SolicitarCodigo202Response{}, nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		// Esto NO filtra nada: dice que lo escrito no tiene forma de correo, no
		// si ese correo existe.
		return api.SolicitarCodigo400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadosEnvios):
		return api.SolicitarCodigo429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx,
				"ya se pidieron varios códigos para ese destino; espera un rato antes de pedir otro"),
		}, nil

	default:
		// Un fallo del sistema SÍ se dice, y no contradice la
		// anti-enumeración: un 500 no depende de si el destino existe, así que
		// no distingue nada sobre él.
		return api.SolicitarCodigo500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "solicitarCodigo", err),
			),
		}, nil
	}
}

func (a *adaptador) CanjearCodigo(
	ctx context.Context, pet api.CanjearCodigoRequestObject,
) (api.CanjearCodigoResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.CanjearCodigo400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	token, expira, err := a.c.Identidad.Canjear(ctx, string(pet.Body.Destino), pet.Body.Codigo)

	switch {
	case err == nil:
		return api.CanjearCodigo200JSONResponse{Token: token, ExpiraEn: expira}, nil

	case errors.Is(err, identidad.ErrCodigoInvalido):
		// Un solo error para el código equivocado, el caducado, el ya usado y
		// el que nunca existió. Cada distinción le diría a quien está probando
		// códigos si va por buen camino.
		return api.CanjearCodigo401ApplicationProblemPlusJSONResponse(
			transporte.NoAutorizado.Cuerpo(ctx, "el código no es válido o ya caducó"),
		), nil

	default:
		return api.CanjearCodigo500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "canjearCodigo", err),
			),
		}, nil
	}
}

// ------------------------------------------------------ consulta (lectura) --

func (a *adaptador) ListarReservas(
	ctx context.Context, pet api.ListarReservasRequestObject,
) (api.ListarReservasResponseObject, error) {
	if a.c.Consulta == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.ListarReservas401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, ""),
			),
		}, nil
	}

	filtro := consulta.Filtro{
		Desde:   pet.Params.Desde,
		Hasta:   pet.Params.Hasta,
		Sede:    pet.Params.SedeId,
		Recurso: pet.Params.RecursoId,
	}
	if pet.Params.Estado != nil {
		filtro.Estados = *pet.Params.Estado
	}
	if pet.Params.Limite != nil {
		filtro.Limite = *pet.Params.Limite
	}
	if pet.Params.Cursor != nil {
		filtro.Cursor = *pet.Params.Cursor
	}

	// Los estados llegan como cadenas del cliente y acaban en un `= ANY` sobre
	// una columna enum. Un valor que no pertenece al enum hace fallar la
	// consulta en el motor, y eso saldría como un 500 cuando en realidad es el
	// cliente el que se equivocó.
	for _, estado := range filtro.Estados {
		if !estado.Valid() {
			return api.ListarReservas400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
					invalida(ctx, "estado desconocido: "+string(estado)),
				),
			}, nil
		}
	}

	// Un agente solo lista si su token lo dice (RF-05/RF-13).
	if acceso, _ := identidad.DeAcceso(ctx); !acceso.Permite(string(api.ListarReservas)) {
		return api.ListarReservas403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				transporte.FueraDeAlcance.Cuerpo(ctx,
					"ese token de agente no incluye listar reservas"),
			),
		}, nil
	}

	lista, err := a.c.Consulta.Listar(ctx, tenantDe(ctx, pet.Params.XTenantId), alcance, filtro)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		if clase.Estado == http.StatusBadRequest {
			return api.ListarReservas400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		}
		return api.ListarReservas500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarReservas", err),
			),
		}, nil
	}

	return api.ListarReservas200JSONResponse(lista), nil
}

func (a *adaptador) ObtenerReserva(
	ctx context.Context, pet api.ObtenerReservaRequestObject,
) (api.ObtenerReservaResponseObject, error) {
	if a.c.Consulta == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.ObtenerReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, ""),
			),
		}, nil
	}

	reserva, err := a.c.Consulta.Obtener(ctx, tenantDe(ctx, pet.Params.XTenantId), alcance, pet.Id)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		if clase.Estado == http.StatusNotFound {
			return api.ObtenerReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		}
		return api.ObtenerReserva500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "obtenerReserva", err),
			),
		}, nil
	}

	return api.ObtenerReserva200JSONResponse(reserva), nil
}
