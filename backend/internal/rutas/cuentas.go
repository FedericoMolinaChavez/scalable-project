package rutas

import (
	"context"
	"errors"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Los manejadores de cuentas, sesiones de cuenta y agentes: RF-12, RF-13,
// RF-18, RF-19, RF-21, RF-22, RF-24 y RF-25.
//
// Están aparte de adaptador.go por tamaño, no por naturaleza: son métodos del
// mismo adaptador y siguen la misma disciplina —extraer, llamar, traducir a uno
// de los códigos que ESA operación declara—. Juntarlos con los de reservas
// dejaría un archivo en el que encontrar un manejador exige buscarlo.

// cuentaDe devuelve el acceso de una ruta que exige cuenta.
//
// Un token de INVITADO no vale aquí, y esa es la comprobación importante: RF-02
// entrega un token que solo acredita un correo, sin cuenta detrás, y las rutas
// de perfil, sesiones y preferencias operan sobre una cuenta que ese token no
// tiene. Sin esta comprobación, un invitado llegaría al servicio con el
// identificador vacío y la consulta acabaría tocando una cuenta sin identificador.
func (a *adaptador) cuentaDe(ctx context.Context) (identidad.Acceso, bool) {
	acceso, hay := identidad.DeAcceso(ctx)
	if !hay {
		a.registro.ErrorContext(ctx, "ruta acotada sin acceso en el contexto",
			slog.String("peticion", transporte.IdPeticion(ctx)))
		return identidad.Acceso{}, false
	}
	return acceso, !acceso.EsInvitado()
}

// ------------------------------------------------------------------ alta --

func (a *adaptador) RegistrarCuenta(
	ctx context.Context, pet api.RegistrarCuentaRequestObject,
) (api.RegistrarCuentaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.RegistrarCuenta400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.Registrar(ctx, *pet.Body)

	switch {
	case err == nil:
		// El mismo 202 exista o no ya ese correo. Es toda la anti-enumeración
		// de RF-24, y por eso el servicio no devuelve nada que la distinga.
		return api.RegistrarCuenta202Response{}, nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		return api.RegistrarCuenta400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrTerminosNoAceptados),
		errors.Is(err, identidad.ErrSinContacto),
		errors.Is(err, identidad.ErrCanalNoSoportado),
		esErrorDeContrasena(err):
		return api.RegistrarCuenta422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				campoDeContrasena(ctx, err),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadosEnvios):
		return api.RegistrarCuenta429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx,
				"se pidieron demasiadas altas para ese correo; espera un rato"),
		}, nil

	default:
		return api.RegistrarCuenta500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "registrarCuenta", err),
			),
		}, nil
	}
}

// ---------------------------------------------------------------- perfil --

func (a *adaptador) ObtenerCuentaPropia(
	ctx context.Context, _ api.ObtenerCuentaPropiaRequestObject,
) (api.ObtenerCuentaPropiaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.ObtenerCuentaPropia403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	cuenta, err := a.c.Identidad.Perfil(ctx, acceso.Cuenta)
	if err != nil {
		return api.ObtenerCuentaPropia500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "obtenerCuentaPropia", err),
			),
		}, nil
	}

	return api.ObtenerCuentaPropia200JSONResponse(cuenta), nil
}

func (a *adaptador) ActualizarCuentaPropia(
	ctx context.Context, pet api.ActualizarCuentaPropiaRequestObject,
) (api.ActualizarCuentaPropiaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.ActualizarCuentaPropia403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}
	if pet.Body == nil {
		return api.ActualizarCuentaPropia400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	cuenta, err := a.c.Identidad.ActualizarPerfil(ctx, acceso.Cuenta, *pet.Body)

	switch {
	case err == nil:
		return api.ActualizarCuentaPropia200JSONResponse(cuenta), nil

	case errors.Is(err, identidad.ErrContactoEnUso):
		return api.ActualizarCuentaPropia409ApplicationProblemPlusJSONResponse(
			transporte.ContactoEnUso.Cuerpo(ctx, err.Error()),
		), nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		return api.ActualizarCuentaPropia400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, err.Error()),
			),
		}, nil

	default:
		return api.ActualizarCuentaPropia500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "actualizarCuentaPropia", err),
			),
		}, nil
	}
}

func (a *adaptador) EliminarCuentaPropia(
	ctx context.Context, _ api.EliminarCuentaPropiaRequestObject,
) (api.EliminarCuentaPropiaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.EliminarCuentaPropia403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	// Un agente NO puede dar de baja la cuenta que representa. Es de las
	// acciones críticas de RF-36, y el alcance que RF-13 concede es una lista
	// cerrada en la que eliminar no está: el token no la lleva y por tanto no
	// la autoriza.
	if acceso.PorAgente() {
		return api.EliminarCuentaPropia403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				transporte.FueraDeAlcance.Cuerpo(ctx,
					"un agente no puede eliminar la cuenta a la que representa"),
			),
		}, nil
	}

	if err := a.c.Identidad.Eliminar(ctx, acceso.Cuenta); err != nil {
		return api.EliminarCuentaPropia500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "eliminarCuentaPropia", err),
			),
		}, nil
	}

	return api.EliminarCuentaPropia204Response{}, nil
}

// ---------------------------------------------------------- verificación --

func (a *adaptador) SolicitarVerificacion(
	ctx context.Context, pet api.SolicitarVerificacionRequestObject,
) (api.SolicitarVerificacionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.SolicitarVerificacion401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}
	if pet.Body == nil {
		return api.SolicitarVerificacion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.SolicitarVerificacion(ctx, acceso.Cuenta, string(pet.Body.Canal))

	switch {
	case err == nil:
		return api.SolicitarVerificacion202Response{}, nil

	case errors.Is(err, identidad.ErrCanalNoSoportado), errors.Is(err, identidad.ErrSinContacto):
		return api.SolicitarVerificacion422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				transporte.ReglaNegocio.Cuerpo(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadosEnvios):
		return api.SolicitarVerificacion429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx,
				"ya se pidieron varias verificaciones; espera un rato antes de pedir otra"),
		}, nil

	default:
		return api.SolicitarVerificacion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "solicitarVerificacion", err),
			),
		}, nil
	}
}

func (a *adaptador) CanjearVerificacion(
	ctx context.Context, pet api.CanjearVerificacionRequestObject,
) (api.CanjearVerificacionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.CanjearVerificacion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	cuenta, err := a.c.Identidad.CanjearVerificacion(ctx, pet.Body.Valor)

	switch {
	case err == nil:
		return api.CanjearVerificacion200JSONResponse(cuenta), nil

	case errors.Is(err, identidad.ErrCodigoInvalido):
		return api.CanjearVerificacion401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "ese enlace ya no vale; pide otro"),
			),
		}, nil

	default:
		return api.CanjearVerificacion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "canjearVerificacion", err),
			),
		}, nil
	}
}

// ------------------------------------------------------------ contraseña --

func (a *adaptador) SolicitarRecuperacion(
	ctx context.Context, pet api.SolicitarRecuperacionRequestObject,
) (api.SolicitarRecuperacionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.SolicitarRecuperacion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.SolicitarRecuperacion(ctx, string(pet.Body.Destino))

	switch {
	case err == nil:
		return api.SolicitarRecuperacion202Response{}, nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		return api.SolicitarRecuperacion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadosEnvios):
		return api.SolicitarRecuperacion429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx,
				"ya se pidieron varios enlaces para ese correo; espera un rato"),
		}, nil

	default:
		return api.SolicitarRecuperacion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "solicitarRecuperacion", err),
			),
		}, nil
	}
}

func (a *adaptador) RestablecerContrasena(
	ctx context.Context, pet api.RestablecerContrasenaRequestObject,
) (api.RestablecerContrasenaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.RestablecerContrasena400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.Restablecer(ctx, pet.Body.Token, pet.Body.ContrasenaNueva)

	switch {
	case err == nil:
		return api.RestablecerContrasena204Response{}, nil

	case errors.Is(err, identidad.ErrCodigoInvalido):
		return api.RestablecerContrasena401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "ese enlace ya no vale; pide otro"),
			),
		}, nil

	case esErrorDeContrasena(err):
		return api.RestablecerContrasena422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				campoDeContrasena(ctx, err),
			),
		}, nil

	default:
		return api.RestablecerContrasena500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "restablecerContrasena", err),
			),
		}, nil
	}
}

func (a *adaptador) CambiarContrasena(
	ctx context.Context, pet api.CambiarContrasenaRequestObject,
) (api.CambiarContrasenaResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta || acceso.PorAgente() {
		// Un agente tampoco cambia contraseñas: no está en el alcance que
		// RF-13 puede conceder, y una credencial de acceso permanente no es
		// algo que se delegue por minutos.
		return api.CambiarContrasena403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}
	if pet.Body == nil {
		return api.CambiarContrasena400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.CambiarContrasena(
		ctx, acceso.Cuenta, acceso.Sesion, pet.Body.ContrasenaActual, pet.Body.ContrasenaNueva)

	switch {
	case err == nil:
		return api.CambiarContrasena204Response{}, nil

	case errors.Is(err, identidad.ErrCredencialesInvalidas):
		return api.CambiarContrasena401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "la contraseña actual no es correcta"),
			),
		}, nil

	case esErrorDeContrasena(err):
		return api.CambiarContrasena422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				campoDeContrasena(ctx, err),
			),
		}, nil

	default:
		return api.CambiarContrasena500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "cambiarContrasena", err),
			),
		}, nil
	}
}

// --------------------------------------------------------- preferencias --

func (a *adaptador) ListarPreferencias(
	ctx context.Context, _ api.ListarPreferenciasRequestObject,
) (api.ListarPreferenciasResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.ListarPreferencias403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	lista, err := a.c.Identidad.Preferencias(ctx, acceso.Cuenta)
	if err != nil {
		return api.ListarPreferencias500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarPreferencias", err),
			),
		}, nil
	}

	return api.ListarPreferencias200JSONResponse(lista), nil
}

func (a *adaptador) GuardarPreferencias(
	ctx context.Context, pet api.GuardarPreferenciasRequestObject,
) (api.GuardarPreferenciasResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.GuardarPreferencias403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}
	if pet.Body == nil {
		return api.GuardarPreferencias400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	lista, err := a.c.Identidad.GuardarPreferencias(ctx, acceso.Cuenta, *pet.Body)

	switch {
	case err == nil:
		return api.GuardarPreferencias200JSONResponse(lista), nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		return api.GuardarPreferencias422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				transporte.ReglaNegocio.Cuerpo(ctx, err.Error()),
			),
		}, nil

	default:
		return api.GuardarPreferencias500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "guardarPreferencias", err),
			),
		}, nil
	}
}

// ------------------------------------------------ sesiones de cuenta --

func (a *adaptador) IniciarSesion(
	ctx context.Context, pet api.IniciarSesionRequestObject,
) (api.IniciarSesionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.IniciarSesion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	cliente := transporte.DeCliente(ctx)
	par, err := a.c.Identidad.IniciarSesion(
		ctx, pet.Body.Identificador, pet.Body.Contrasena,
		identidad.ContextoSesion{Dispositivo: cliente.Dispositivo, IP: cliente.IP})

	switch {
	case err == nil:
		return api.IniciarSesion200JSONResponse(par), nil

	case errors.Is(err, identidad.ErrCredencialesInvalidas):
		return api.IniciarSesion401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "el correo o la contraseña no son correctos"),
			),
		}, nil

	case errors.Is(err, identidad.ErrCuentaNoActiva):
		// SÍ se dice cuál es el estado (RF-12 A8), y no contradice la
		// anti-enumeración: quien llega hasta aquí ya acertó la contraseña.
		return api.IniciarSesion422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				transporte.ReglaNegocio.Cuerpo(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrCuentaBloqueada):
		return api.IniciarSesion429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx, err.Error()),
		}, nil

	default:
		return api.IniciarSesion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "iniciarSesion", err),
			),
		}, nil
	}
}

func (a *adaptador) SolicitarEnlace(
	ctx context.Context, pet api.SolicitarEnlaceRequestObject,
) (api.SolicitarEnlaceResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.SolicitarEnlace400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	err := a.c.Identidad.SolicitarEnlaceEntrada(ctx, string(pet.Body.Destino))

	switch {
	case err == nil:
		return api.SolicitarEnlace202Response{}, nil

	case errors.Is(err, identidad.ErrDestinoInvalido):
		return api.SolicitarEnlace400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadosEnvios):
		return api.SolicitarEnlace429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx,
				"ya se pidieron varios enlaces para ese correo; espera un rato"),
		}, nil

	default:
		return api.SolicitarEnlace500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "solicitarEnlace", err),
			),
		}, nil
	}
}

func (a *adaptador) CanjearEnlace(
	ctx context.Context, pet api.CanjearEnlaceRequestObject,
) (api.CanjearEnlaceResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.CanjearEnlace400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	cliente := transporte.DeCliente(ctx)
	par, err := a.c.Identidad.CanjearEnlaceEntrada(ctx, pet.Body.Token,
		identidad.ContextoSesion{Dispositivo: cliente.Dispositivo, IP: cliente.IP})

	switch {
	case err == nil:
		return api.CanjearEnlace200JSONResponse(par), nil

	case errors.Is(err, identidad.ErrCodigoInvalido):
		return api.CanjearEnlace401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, "ese enlace ya no vale; pide otro"),
			),
		}, nil

	case errors.Is(err, identidad.ErrCuentaNoActiva):
		return api.CanjearEnlace422ApplicationProblemPlusJSONResponse{
			NoProcesableApplicationProblemPlusJSONResponse: api.NoProcesableApplicationProblemPlusJSONResponse(
				transporte.ReglaNegocio.Cuerpo(ctx, err.Error()),
			),
		}, nil

	default:
		return api.CanjearEnlace500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "canjearEnlace", err),
			),
		}, nil
	}
}

func (a *adaptador) RefrescarSesion(
	ctx context.Context, pet api.RefrescarSesionRequestObject,
) (api.RefrescarSesionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.RefrescarSesion400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	cliente := transporte.DeCliente(ctx)
	par, err := a.c.Identidad.Refrescar(ctx, pet.Body.Refresco,
		identidad.ContextoSesion{Dispositivo: cliente.Dispositivo, IP: cliente.IP})

	switch {
	case err == nil:
		return api.RefrescarSesion200JSONResponse(par), nil

	case errors.Is(err, identidad.ErrRefrescoInvalido):
		return api.RefrescarSesion401ApplicationProblemPlusJSONResponse{
			NoAutorizadoApplicationProblemPlusJSONResponse: api.NoAutorizadoApplicationProblemPlusJSONResponse(
				transporte.NoAutorizado.Cuerpo(ctx, err.Error()),
			),
		}, nil

	default:
		return api.RefrescarSesion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "refrescarSesion", err),
			),
		}, nil
	}
}

func (a *adaptador) ListarSesiones(
	ctx context.Context, _ api.ListarSesionesRequestObject,
) (api.ListarSesionesResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.ListarSesiones403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	lista, err := a.c.Identidad.ListarSesiones(ctx, acceso.Cuenta, acceso.Sesion)
	if err != nil {
		return api.ListarSesiones500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "listarSesiones", err),
			),
		}, nil
	}

	return api.ListarSesiones200JSONResponse(lista), nil
}

func (a *adaptador) RevocarSesion(
	ctx context.Context, pet api.RevocarSesionRequestObject,
) (api.RevocarSesionResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.RevocarSesion403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	err := a.c.Identidad.RevocarSesion(ctx, acceso.Cuenta, pet.Id.String())

	switch {
	case err == nil:
		return api.RevocarSesion204Response{}, nil

	case errors.Is(err, datos.ErrNoEncontrado):
		// Una sesión de otra cuenta responde igual que una inexistente: un 403
		// confirmaría que ese identificador existe.
		return api.RevocarSesion404ApplicationProblemPlusJSONResponse{
			NoEncontradoApplicationProblemPlusJSONResponse: api.NoEncontradoApplicationProblemPlusJSONResponse(
				transporte.NoEncontrado.Cuerpo(ctx, ""),
			),
		}, nil

	default:
		return api.RevocarSesion500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "revocarSesion", err),
			),
		}, nil
	}
}

func (a *adaptador) RevocarTodasLasSesiones(
	ctx context.Context, _ api.RevocarTodasLasSesionesRequestObject,
) (api.RevocarTodasLasSesionesResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}

	acceso, hayCuenta := a.cuentaDe(ctx)
	if !hayCuenta {
		return api.RevocarTodasLasSesiones403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				sinCuenta(ctx),
			),
		}, nil
	}

	if err := a.c.Identidad.RevocarTodasLasSesiones(ctx, acceso.Cuenta); err != nil {
		return api.RevocarTodasLasSesiones500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "revocarTodasLasSesiones", err),
			),
		}, nil
	}

	return api.RevocarTodasLasSesiones204Response{}, nil
}

// ---------------------------------------------------------------- agente --

func (a *adaptador) IntercambiarTokenAgente(
	ctx context.Context, pet api.IntercambiarTokenAgenteRequestObject,
) (api.IntercambiarTokenAgenteResponseObject, error) {
	if a.c.Identidad == nil {
		return nil, errSinComponente
	}
	if pet.Body == nil {
		return api.IntercambiarTokenAgente400ApplicationProblemPlusJSONResponse{
			PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
				invalida(ctx, "falta el cuerpo de la petición"),
			),
		}, nil
	}

	for _, accion := range pet.Body.Acciones {
		if !accion.Valid() {
			return api.IntercambiarTokenAgente400ApplicationProblemPlusJSONResponse{
				PeticionInvalidaApplicationProblemPlusJSONResponse: api.PeticionInvalidaApplicationProblemPlusJSONResponse(
					invalida(ctx, "acción desconocida: "+string(accion)),
				),
			}, nil
		}
	}

	token, err := a.c.Identidad.IntercambiarTokenAgente(
		ctx, pet.Body.Credencial, pet.Body.CuentaId, pet.Body.Acciones)

	switch {
	case err == nil:
		return api.IntercambiarTokenAgente200JSONResponse(token), nil

	case errors.Is(err, identidad.ErrAgenteNoAutorizado):
		// Las tres razones de RF-13 responden igual: separarlas le diría a
		// quien prueba credenciales qué parte acertó.
		return api.IntercambiarTokenAgente403ApplicationProblemPlusJSONResponse{
			FueraDeAlcanceApplicationProblemPlusJSONResponse: api.FueraDeAlcanceApplicationProblemPlusJSONResponse(
				transporte.FueraDeAlcance.Cuerpo(ctx, err.Error()),
			),
		}, nil

	case errors.Is(err, identidad.ErrDemasiadasAcciones):
		return api.IntercambiarTokenAgente429ApplicationProblemPlusJSONResponse{
			Body: transporte.DemasiadasPeticiones.Cuerpo(ctx, err.Error()),
		}, nil

	default:
		return api.IntercambiarTokenAgente500ApplicationProblemPlusJSONResponse{
			ErrorInternoApplicationProblemPlusJSONResponse: api.ErrorInternoApplicationProblemPlusJSONResponse(
				a.interno(ctx, "intercambiarTokenAgente", err),
			),
		}, nil
	}
}

// ----------------------------------------------------------- auxiliares --

// sinCuenta es el cuerpo de "esto necesita una cuenta y tú traes un token de
// invitado".
func sinCuenta(ctx context.Context) api.Problema {
	return transporte.FueraDeAlcance.Cuerpo(ctx,
		"esta operación es de una cuenta; un código de consulta (RF-02) no la acredita")
}

func esErrorDeContrasena(err error) bool {
	return errors.Is(err, dominio.ErrContrasenaCorta) ||
		errors.Is(err, dominio.ErrContrasenaLarga) ||
		errors.Is(err, dominio.ErrContrasenaPrevisible)
}

// campoDeContrasena atribuye el error al campo concreto.
//
// El detalle por campo existe para que la interfaz pueda marcar el input que
// falla en vez de mostrar un mensaje suelto encima del formulario. Sin él, un
// alta con tres campos y un solo mensaje obliga a adivinar cuál era.
func campoDeContrasena(ctx context.Context, err error) api.Problema {
	if !esErrorDeContrasena(err) {
		return transporte.ReglaNegocio.Cuerpo(ctx, err.Error())
	}

	return transporte.ReglaNegocio.CuerpoConCampos(ctx, err.Error(),
		transporte.Campo{Nombre: "contrasena", Mensaje: err.Error()})
}
