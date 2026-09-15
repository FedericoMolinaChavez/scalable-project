package rutas

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Los tres manejadores del componente de pagos (RF-01, RF-33, RF-34).
//
// Viven en un archivo aparte del resto del adaptador porque los sirven dos
// binarios distintos: la intención y el estado son de `pagos`, y el
// comprobante es de `consulta`, que es quien lee. Tenerlos juntos deja a la
// vista que la separación no es por familia de rutas sino por quién escribe y
// quién lee, que es la regla de ARQ-01.

func (a *adaptador) CrearIntencionPago(
	ctx context.Context, pet api.CrearIntencionPagoRequestObject,
) (api.CrearIntencionPagoResponseObject, error) {
	if a.c.Pagos == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.CrearIntencionPago400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	intento, err := a.c.Pagos.Intencion(ctx, pet.Params.XTenantId, pet.Body.ReservaId)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusNotFound:
			return api.CrearIntencionPago404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			return api.CrearIntencionPago409ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusBadGateway:
			// El detalle del fallo de Stripe NO sale hacia el cliente —puede
			// llevar identificadores internos de la cuenta— pero el registro sí
			// lo conserva. Lo que el cliente necesita saber es de qué lado está
			// el problema, y eso lo dice el 502.
			a.registro.ErrorContext(ctx, "el proveedor de pago falló",
				slog.String("operacion", "crearIntencionPago"),
				slog.String("error", err.Error()),
				slog.String("peticion", transporte.IdPeticion(ctx)))
			return api.CrearIntencionPago502ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusBadRequest:
			return api.CrearIntencionPago400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.CrearIntencionPago500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "crearIntencionPago", err),
				),
			}, nil
		}
	}

	return api.CrearIntencionPago200JSONResponse{
		ClientSecret:    intento.ClientSecret,
		ClavePublicable: a.c.Pagos.ClavePublicable(),
		Monto:           api.Dinero{Monto: intento.Monto.Texto(), Moneda: intento.Monto.Moneda},
		Estado:          api.EstadoPago(intento.Estado),
		ExpiraEn:        intento.ExpiraEn,
	}, nil
}

func (a *adaptador) ConsultarConfirmacion(
	ctx context.Context, pet api.ConsultarConfirmacionRequestObject,
) (api.ConsultarConfirmacionResponseObject, error) {
	if a.c.Pagos == nil {
		return nil, errSinComponente
	}

	confirmacion, err := a.c.Pagos.Estado(ctx, pet.Params.XTenantId, pet.ReservaId)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		if clase.Estado == http.StatusNotFound {
			return api.ConsultarConfirmacion404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		}
		return api.ConsultarConfirmacion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "consultarConfirmacion", err),
			),
		}, nil
	}

	respuesta := api.ConsultarConfirmacion200JSONResponse{
		ReservaEstado: api.EstadoReserva(confirmacion.ReservaEstado),
	}
	if confirmacion.PagoEstado != "" {
		estado := api.EstadoPago(confirmacion.PagoEstado)
		respuesta.PagoEstado = &estado
	}
	if confirmacion.MotivoFallo != "" {
		respuesta.MotivoFallo = &confirmacion.MotivoFallo
	}

	return respuesta, nil
}

func (a *adaptador) ObtenerComprobante(
	ctx context.Context, pet api.ObtenerComprobanteRequestObject,
) (api.ObtenerComprobanteResponseObject, error) {
	if a.c.Comprobantes == nil {
		return nil, errSinComponente
	}

	acceso, hay := identidad.DeAcceso(ctx)
	if !hay || acceso.Destino == "" {
		a.registro.ErrorContext(ctx, "ruta acotada sin acceso en el contexto",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return api.ObtenerComprobante401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, ""),
			),
		}, nil
	}

	documento, err := a.c.Comprobantes.Comprobante(ctx, pet.Params.XTenantId, pet.Id, acceso.Destino)
	if err != nil {
		if errors.Is(err, pagos.ErrSinComprobante) {
			return api.ObtenerComprobante404ApplicationProblemPlusJSONResponse(
				transporte.NoEncontrado.Cuerpo(ctx, ""),
			), nil
		}
		return api.ObtenerComprobante500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "obtenerComprobante", err),
			),
		}, nil
	}

	respuesta := api.ObtenerComprobante200JSONResponse{
		Numero:      documento.Numero,
		Tipo:        api.ComprobanteTipo(documento.Tipo),
		EmitidoEn:   documento.EmitidoEn,
		UrlExpiraEn: documento.URLExpiraEn,
	}
	if documento.URL != "" {
		respuesta.Url = &documento.URL
	}

	return respuesta, nil
}
