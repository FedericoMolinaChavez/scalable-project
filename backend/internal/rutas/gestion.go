package rutas

import (
	"context"
	"net/http"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Lo que se le puede hacer a una reserva que ya existe: moverla (RF-07),
// registrar una transición manual (RF-28 / RF-32) y calificarla (RF-20).
//
// Las tres son del núcleo por la misma razón que crear y cancelar: escriben en
// negocio.reserva o dependen de su estado dentro de la misma transacción.

func (a *adaptador) ModificarReserva(
	ctx context.Context, pet api.ModificarReservaRequestObject,
) (api.ModificarReservaResponseObject, error) {
	if a.c.Nucleo == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.ModificarReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "")),
		}, nil
	}
	if pet.Body == nil {
		return api.ModificarReserva400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	// Un agente reprograma solo si su token lo dice. Mover una cita es
	// disponer del cupo de alguien, así que se apoya en el mismo permiso que
	// reservar (RF-13): quien puede tomar un horario en su nombre puede
	// cambiárselo, y quien no, tampoco.
	acceso, _ := identidad.DeAcceso(ctx)
	if !acceso.Permite(string(api.Reservar)) {
		return api.ModificarReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx,
					"ese token de agente no incluye reservar, así que tampoco reprogramar")),
		}, nil
	}

	reserva, err := a.c.Nucleo.Modificar(
		ctx, tenantDe(ctx, pet.Params.XTenantId), pet.Id,
		alcance, auditoria.DeContexto(ctx), *pet.Body)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusBadRequest:
			return api.ModificarReserva400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusNotFound:
			// Incluye la reserva que existe pero es de otra persona.
			return api.ModificarReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			// Dos cosas distintas con el mismo código, y las dos son
			// conflictos con el estado del mundo: el horario nuevo ya está
			// tomado, o la reserva ya no se puede mover. El `type` del
			// problema las distingue.
			return api.ModificarReserva409ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusUnprocessableEntity, http.StatusForbidden:
			return api.ModificarReserva422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.ModificarReserva500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "modificarReserva", err)),
			}, nil
		}
	}

	return api.ModificarReserva200JSONResponse(reserva), nil
}

func (a *adaptador) CambiarEstadoReserva(
	ctx context.Context, pet api.CambiarEstadoReservaRequestObject,
) (api.CambiarEstadoReservaResponseObject, error) {
	if a.c.Nucleo == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.CambiarEstadoReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "")),
		}, nil
	}
	if pet.Body == nil {
		return api.CambiarEstadoReserva400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}
	if !pet.Body.Estado.Valid() {
		// El estado llega como cadena y acaba en un cast a un enum del motor.
		// Uno desconocido haría fallar la consulta, y eso saldría como un 500
		// cuando el que se equivocó fue el cliente.
		return api.CambiarEstadoReserva400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "estado desconocido: "+string(pet.Body.Estado))),
		}, nil
	}

	reserva, err := a.c.Nucleo.CambiarEstado(
		ctx, tenantDe(ctx, pet.Params.XTenantId), pet.Id,
		alcance, auditoria.DeContexto(ctx), *pet.Body)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusForbidden:
			// Quien no administra este tenant no puede registrar un check-in
			// aunque la reserva sea suya: no es un problema de qué reserva es,
			// es de quién hace el gesto.
			return api.CambiarEstadoReserva403ApplicationProblemPlusJSONResponse{
				FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
					transporte.FueraDeAlcance.Cuerpo(ctx,
						"registrar una transición es de la agenda del negocio (RF-32)")),
			}, nil
		case http.StatusNotFound:
			return api.CambiarEstadoReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			return api.CambiarEstadoReserva409ApplicationProblemPlusJSONResponse(cuerpo), nil
		default:
			return api.CambiarEstadoReserva500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "cambiarEstadoReserva", err)),
			}, nil
		}
	}

	return api.CambiarEstadoReserva200JSONResponse(reserva), nil
}

func (a *adaptador) CalificarReserva(
	ctx context.Context, pet api.CalificarReservaRequestObject,
) (api.CalificarReservaResponseObject, error) {
	if a.c.Nucleo == nil {
		return nil, errSinComponente
	}

	alcance, listo := a.alcance(ctx)
	if !listo {
		return api.CalificarReserva401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "")),
		}, nil
	}
	if pet.Body == nil {
		return api.CalificarReserva400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	calificacion, err := a.c.Nucleo.Calificar(
		ctx, tenantDe(ctx, pet.Params.XTenantId), pet.Id, alcance, *pet.Body)
	if err != nil {
		clase, cuerpo := problema(ctx, err)
		switch clase.Estado {
		case http.StatusBadRequest:
			return api.CalificarReserva400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusNotFound:
			return api.CalificarReserva404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		case http.StatusConflict:
			return api.CalificarReserva409ApplicationProblemPlusJSONResponse(cuerpo), nil
		case http.StatusUnprocessableEntity, http.StatusForbidden:
			return api.CalificarReserva422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(cuerpo),
			}, nil
		default:
			return api.CalificarReserva500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "calificarReserva", err)),
			}, nil
		}
	}

	return api.CalificarReserva201JSONResponse(calificacion), nil
}
