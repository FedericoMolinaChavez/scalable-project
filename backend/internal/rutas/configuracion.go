package rutas

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Los manejadores de la configuración del negocio: RF-14, RF-15, RF-17, RF-30,
// RF-31, y la consulta de RF-36.
//
// Todos empiezan igual —comprobar que quien pide administra un tenant— y todos
// pasan el mismo actor al componente, que lo escribe en la auditoría dentro de
// la transacción. Esa uniformidad es intencionada: la alternativa es que cada
// manejador decida por su cuenta si audita, y el que se olvide no fallará.

// administrador resuelve el tenant y el actor de una ruta de configuración.
//
// El tenant sale del TOKEN y no de una cabecera, y esa es la comprobación que
// de verdad importa aquí: `X-Tenant-Id` la escribe quien llama, así que
// aceptarla en una ruta de escritura permitiría configurar el negocio de otro
// escribiendo su identificador.
//
// Un agente no pasa aunque represente a un administrador. RF-13 concede
// acciones sobre UNA cuenta, y "reconfigurar el negocio entero" no es una
// acción sobre una cuenta.
func (a *adaptador) administrador(ctx context.Context) (uuid.UUID, auditoria.Actor, bool) {
	acceso, hay := identidad.DeAcceso(ctx)
	if !hay {
		a.registro.ErrorContext(ctx, "ruta de configuración sin acceso en el contexto",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return uuid.Nil, auditoria.Actor{}, false
	}

	if acceso.Tipo != identidad.TipoAdmin || acceso.PorAgente() || acceso.Tenant == "" {
		return uuid.Nil, auditoria.Actor{}, false
	}

	tenant, err := uuid.Parse(acceso.Tenant)
	if err != nil {
		// Un tenant ilegible dentro de un token que nosotros firmamos es un
		// fallo nuestro. Se cae del lado seguro: sin tenant no se configura
		// nada.
		a.registro.ErrorContext(ctx, "token de administrador con tenant ilegible",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return uuid.Nil, auditoria.Actor{}, false
	}

	return tenant, auditoria.DeContexto(ctx), true
}

// sinAdministrador es el cuerpo de "esto lo configura el dueño del negocio".
func sinAdministrador(ctx context.Context) api.Problema {
	return transporte.FueraDeAlcance.Cuerpo(ctx,
		"esta operación la hace el administrador del negocio (RF-23)")
}

// claseDeConfiguracion traduce los errores de la configuración a un estado.
//
// Es la misma disciplina que clasificar() y va aparte porque los errores son
// otros: aquí no hay solapamientos de cupo ni políticas de cancelación, hay
// referencias a filas de otro tenant, restricciones de forma y filas que no
// existen.
func claseDeConfiguracion(err error) (transporte.Clase, string) {
	switch {
	case errors.Is(err, datos.ErrNoEncontrado):
		return transporte.NoEncontrado, ""

	case errors.Is(err, datos.ErrDuplicado):
		return transporte.ContactoEnUso, "ya existe un registro con ese identificador"

	case errors.Is(err, catalogo.ErrExcepcionSinObjetivo),
		errors.Is(err, dominio.ErrPeriodoInvalido),
		errors.Is(err, datos.ErrReferenciaInvalida),
		errors.Is(err, datos.ErrRestriccion):
		return transporte.ReglaNegocio, err.Error()

	default:
		return transporte.Interno, ""
	}
}

// ------------------------------------------------------------------ sedes --

func (a *adaptador) ListarSedesConfig(
	ctx context.Context, _ api.ListarSedesConfigRequestObject,
) (api.ListarSedesConfigResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarSedesConfig403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	sedes, err := a.c.Catalogo.SedesTodas(ctx, tenant)
	if err != nil {
		return api.ListarSedesConfig500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarSedesConfig", err)),
		}, nil
	}

	return api.ListarSedesConfig200JSONResponse{Datos: sedes}, nil
}

func (a *adaptador) CrearSede(
	ctx context.Context, pet api.CrearSedeRequestObject,
) (api.CrearSedeResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearSede403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearSede400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	sede, err := a.c.Catalogo.CrearSede(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearSede422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearSede500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearSede", err)),
		}, nil
	}

	return api.CrearSede201JSONResponse(sede), nil
}

func (a *adaptador) ActualizarSede(
	ctx context.Context, pet api.ActualizarSedeRequestObject,
) (api.ActualizarSedeResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ActualizarSede403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.ActualizarSede400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	sede, err := a.c.Catalogo.ActualizarSede(ctx, tenant, actor, pet.Id, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		switch clase.Estado {
		case http.StatusNotFound:
			return api.ActualizarSede404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		case http.StatusUnprocessableEntity:
			return api.ActualizarSede422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		default:
			return api.ActualizarSede500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "actualizarSede", err)),
			}, nil
		}
	}

	return api.ActualizarSede200JSONResponse(sede), nil
}

// -------------------------------------------------------------- servicios --

func (a *adaptador) ListarServiciosConfig(
	ctx context.Context, _ api.ListarServiciosConfigRequestObject,
) (api.ListarServiciosConfigResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarServiciosConfig403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	servicios, err := a.c.Catalogo.ServiciosTodos(ctx, tenant)
	if err != nil {
		return api.ListarServiciosConfig500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarServiciosConfig", err)),
		}, nil
	}

	return api.ListarServiciosConfig200JSONResponse{Datos: servicios}, nil
}

func (a *adaptador) CrearServicio(
	ctx context.Context, pet api.CrearServicioRequestObject,
) (api.CrearServicioResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearServicio403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearServicio400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	servicio, err := a.c.Catalogo.CrearServicio(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearServicio422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearServicio500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearServicio", err)),
		}, nil
	}

	return api.CrearServicio201JSONResponse(servicio), nil
}

func (a *adaptador) ActualizarServicio(
	ctx context.Context, pet api.ActualizarServicioRequestObject,
) (api.ActualizarServicioResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ActualizarServicio403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.ActualizarServicio400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	servicio, err := a.c.Catalogo.ActualizarServicio(ctx, tenant, actor, pet.Id, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		switch clase.Estado {
		case http.StatusNotFound:
			return api.ActualizarServicio404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		case http.StatusUnprocessableEntity:
			return api.ActualizarServicio422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		default:
			return api.ActualizarServicio500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "actualizarServicio", err)),
			}, nil
		}
	}

	return api.ActualizarServicio200JSONResponse(servicio), nil
}

// --------------------------------------------------------------- recursos --

func (a *adaptador) ListarRecursos(
	ctx context.Context, _ api.ListarRecursosRequestObject,
) (api.ListarRecursosResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarRecursos403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	recursos, err := a.c.Catalogo.Recursos(ctx, tenant)
	if err != nil {
		return api.ListarRecursos500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarRecursos", err)),
		}, nil
	}

	return api.ListarRecursos200JSONResponse{Datos: recursos}, nil
}

func (a *adaptador) CrearRecurso(
	ctx context.Context, pet api.CrearRecursoRequestObject,
) (api.CrearRecursoResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearRecurso403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearRecurso400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	recurso, err := a.c.Catalogo.CrearRecurso(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearRecurso422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearRecurso500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearRecurso", err)),
		}, nil
	}

	return api.CrearRecurso201JSONResponse(recurso), nil
}

func (a *adaptador) ActualizarRecurso(
	ctx context.Context, pet api.ActualizarRecursoRequestObject,
) (api.ActualizarRecursoResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ActualizarRecurso403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.ActualizarRecurso400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	recurso, err := a.c.Catalogo.ActualizarRecurso(ctx, tenant, actor, pet.Id, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		switch clase.Estado {
		case http.StatusNotFound:
			return api.ActualizarRecurso404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		case http.StatusUnprocessableEntity:
			return api.ActualizarRecurso422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		default:
			return api.ActualizarRecurso500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "actualizarRecurso", err)),
			}, nil
		}
	}

	return api.ActualizarRecurso200JSONResponse(recurso), nil
}

// ----------------------------------------------------------------- reglas --

func (a *adaptador) ListarReglas(
	ctx context.Context, pet api.ListarReglasRequestObject,
) (api.ListarReglasResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarReglas403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	reglas, err := a.c.Catalogo.Reglas(ctx, tenant, pet.Params.RecursoId)
	if err != nil {
		return api.ListarReglas500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarReglas", err)),
		}, nil
	}

	return api.ListarReglas200JSONResponse{Datos: reglas}, nil
}

func (a *adaptador) CrearRegla(
	ctx context.Context, pet api.CrearReglaRequestObject,
) (api.CrearReglaResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearRegla403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearRegla400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	regla, err := a.c.Catalogo.CrearRegla(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearRegla422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearRegla500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearRegla", err)),
		}, nil
	}

	return api.CrearRegla201JSONResponse(regla), nil
}

func (a *adaptador) EliminarRegla(
	ctx context.Context, pet api.EliminarReglaRequestObject,
) (api.EliminarReglaResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.EliminarRegla403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	if err := a.c.Catalogo.EliminarRegla(ctx, tenant, actor, pet.Id); err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusNotFound {
			return api.EliminarRegla404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.EliminarRegla500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "eliminarRegla", err)),
		}, nil
	}

	return api.EliminarRegla204Response{}, nil
}

// ------------------------------------------------------------ excepciones --

func (a *adaptador) ListarExcepciones(
	ctx context.Context, _ api.ListarExcepcionesRequestObject,
) (api.ListarExcepcionesResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarExcepciones403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	excepciones, err := a.c.Catalogo.Excepciones(ctx, tenant)
	if err != nil {
		return api.ListarExcepciones500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarExcepciones", err)),
		}, nil
	}

	return api.ListarExcepciones200JSONResponse{Datos: excepciones}, nil
}

func (a *adaptador) CrearExcepcion(
	ctx context.Context, pet api.CrearExcepcionRequestObject,
) (api.CrearExcepcionResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearExcepcion403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearExcepcion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	excepcion, err := a.c.Catalogo.CrearExcepcion(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearExcepcion422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearExcepcion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearExcepcion", err)),
		}, nil
	}

	return api.CrearExcepcion201JSONResponse(excepcion), nil
}

func (a *adaptador) EliminarExcepcion(
	ctx context.Context, pet api.EliminarExcepcionRequestObject,
) (api.EliminarExcepcionResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.EliminarExcepcion403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	if err := a.c.Catalogo.EliminarExcepcion(ctx, tenant, actor, pet.Id); err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusNotFound {
			return api.EliminarExcepcion404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.EliminarExcepcion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "eliminarExcepcion", err)),
		}, nil
	}

	return api.EliminarExcepcion204Response{}, nil
}

// -------------------------------------------------------------- políticas --

func (a *adaptador) ListarPoliticas(
	ctx context.Context, _ api.ListarPoliticasRequestObject,
) (api.ListarPoliticasResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarPoliticas403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	politicas, err := a.c.Catalogo.Politicas(ctx, tenant)
	if err != nil {
		return api.ListarPoliticas500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarPoliticas", err)),
		}, nil
	}

	return api.ListarPoliticas200JSONResponse{Datos: politicas}, nil
}

func (a *adaptador) PublicarPolitica(
	ctx context.Context, pet api.PublicarPoliticaRequestObject,
) (api.PublicarPoliticaResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.PublicarPolitica403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.PublicarPolitica400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	politica, err := a.c.Catalogo.PublicarPolitica(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.PublicarPolitica422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.PublicarPolitica500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "publicarPolitica", err)),
		}, nil
	}

	return api.PublicarPolitica201JSONResponse(politica), nil
}

// --------------------------------------------------------------- vouchers --

func (a *adaptador) ListarVouchers(
	ctx context.Context, _ api.ListarVouchersRequestObject,
) (api.ListarVouchersResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarVouchers403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	vouchers, err := a.c.Catalogo.Vouchers(ctx, tenant)
	if err != nil {
		return api.ListarVouchers500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarVouchers", err)),
		}, nil
	}

	return api.ListarVouchers200JSONResponse{Datos: vouchers}, nil
}

func (a *adaptador) CrearVoucher(
	ctx context.Context, pet api.CrearVoucherRequestObject,
) (api.CrearVoucherResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearVoucher403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearVoucher400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	voucher, err := a.c.Catalogo.CrearVoucher(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		switch clase.Estado {
		case http.StatusConflict:
			return api.CrearVoucher409ApplicationProblemPlusJSONResponse(
				transporte.ContactoEnUso.Cuerpo(ctx,
					"ya existe un voucher con ese código; los códigos no se reutilizan")), nil
		case http.StatusUnprocessableEntity:
			return api.CrearVoucher422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		default:
			return api.CrearVoucher500ApplicationProblemPlusJSONResponse{
				ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
					a.interno(ctx, "crearVoucher", err)),
			}, nil
		}
	}

	return api.CrearVoucher201JSONResponse(voucher), nil
}

func (a *adaptador) EliminarVoucher(
	ctx context.Context, pet api.EliminarVoucherRequestObject,
) (api.EliminarVoucherResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.EliminarVoucher403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	if err := a.c.Catalogo.EliminarVoucher(ctx, tenant, actor, pet.Id); err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusNotFound {
			return api.EliminarVoucher404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.EliminarVoucher500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "eliminarVoucher", err)),
		}, nil
	}

	return api.EliminarVoucher204Response{}, nil
}

// ---------------------------------------------------------------- tarifas --

func (a *adaptador) ListarTarifas(
	ctx context.Context, pet api.ListarTarifasRequestObject,
) (api.ListarTarifasResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ListarTarifas403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	tarifas, err := a.c.Catalogo.Tarifas(ctx, tenant, pet.Params.ServicioId)
	if err != nil {
		return api.ListarTarifas500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarTarifas", err)),
		}, nil
	}

	return api.ListarTarifas200JSONResponse{Datos: tarifas}, nil
}

func (a *adaptador) CrearTarifa(
	ctx context.Context, pet api.CrearTarifaRequestObject,
) (api.CrearTarifaResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.CrearTarifa403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}
	if pet.Body == nil {
		return api.CrearTarifa400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición")),
		}, nil
	}

	tarifa, err := a.c.Catalogo.CrearTarifa(ctx, tenant, actor, *pet.Body)
	if err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusUnprocessableEntity {
			return api.CrearTarifa422ApplicationProblemPlusJSONResponse{
				NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.CrearTarifa500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "crearTarifa", err)),
		}, nil
	}

	return api.CrearTarifa201JSONResponse(tarifa), nil
}

func (a *adaptador) EliminarTarifa(
	ctx context.Context, pet api.EliminarTarifaRequestObject,
) (api.EliminarTarifaResponseObject, error) {
	if a.c.Catalogo == nil {
		return nil, errSinComponente
	}

	tenant, actor, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.EliminarTarifa403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	if err := a.c.Catalogo.EliminarTarifa(ctx, tenant, actor, pet.Id); err != nil {
		clase, detalle := claseDeConfiguracion(err)
		if clase.Estado == http.StatusNotFound {
			return api.EliminarTarifa404ApplicationProblemPlusJSONResponse{
				NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
					clase.Cuerpo(ctx, detalle)),
			}, nil
		}
		return api.EliminarTarifa500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "eliminarTarifa", err)),
		}, nil
	}

	return api.EliminarTarifa204Response{}, nil
}

// -------------------------------------------------------------- auditoría --

func (a *adaptador) ConsultarAuditoria(
	ctx context.Context, pet api.ConsultarAuditoriaRequestObject,
) (api.ConsultarAuditoriaResponseObject, error) {
	if a.c.Auditoria == nil {
		return nil, errSinComponente
	}

	tenant, _, esAdmin := a.administrador(ctx)
	if !esAdmin {
		return api.ConsultarAuditoria403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinAdministrador(ctx)),
		}, nil
	}

	filtro := auditoria.Filtro{
		Desde: pet.Params.Desde,
		Hasta: pet.Params.Hasta,
		Actor: pet.Params.ActorId,
	}
	if pet.Params.RecursoTipo != nil {
		filtro.RecursoTipo = *pet.Params.RecursoTipo
	}
	if pet.Params.Accion != nil {
		filtro.Accion = *pet.Params.Accion
	}
	if pet.Params.Limite != nil {
		filtro.Limite = *pet.Params.Limite
	}
	if pet.Params.Cursor != nil {
		filtro.Cursor = *pet.Params.Cursor
	}

	lista, err := a.c.Auditoria.Listar(ctx, tenant, filtro)
	if err != nil {
		if errors.Is(err, auditoria.ErrCursorInvalido) {
			return api.ConsultarAuditoria400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
					invalida(ctx, err.Error())),
			}, nil
		}
		return api.ConsultarAuditoria500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "consultarAuditoria", err)),
		}, nil
	}

	return api.ConsultarAuditoria200JSONResponse(lista), nil
}
