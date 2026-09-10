package identidad_test

import (
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Las pruebas de cuentas corren contra la base real, igual que el resto.
//
// No se sustituye por un doble porque lo que se comprueba vive EN PostgreSQL:
// que dos altas con el mismo correo no puedan crear dos cuentas (el índice
// único parcial), que el token de un solo uso no se pueda canjear dos veces
// (FOR UPDATE dentro de la transacción), que rotar el refresco sea atómico, y
// que la anonimización de RF-25 quepa en los CHECK de la tabla. Un doble en
// memoria haría pasar todo eso sin comprobar nada.
//
// Las cuentas que crean NO se borran: plataforma.cuenta no admite DELETE ni
// para el rol de la aplicación, porque los comprobantes de RF-34 la
// referencian. Por eso cada prueba usa un correo único, igual que las de RF-02.

const contrasenaBuena = "una-frase-larga-de-prueba"

func servicioCuentas(t *testing.T) (*identidad.Servicio, *buzon) {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	b := &buzon{}

	return identidad.Nuevo(
		bd, b, identidad.NuevoFirmante([]byte(secreto), 15*time.Minute),
		limitador(t), slog.New(slog.NewTextHandler(io.Discard, nil)),
		identidad.Opciones{
			TTLCodigo:        5 * time.Minute,
			TTLEnlace:        15 * time.Minute,
			TTLRefresco:      24 * time.Hour,
			MaxIntentos:      3,
			MaxEnviosHora:    10,
			MaxIntentosLogin: 3,
			BloqueoLogin:     time.Minute,
			BaseURL:          "http://app.test",
		},
	), b
}

// ultimoEnlace saca el token del último correo enviado.
//
// Se busca por el parámetro y no por la ruta: las tres rutas de enlace
// —verificar, entrar, restablecer— son distintas, y una prueba que fijara una
// de ellas dejaría de valer al cambiar el nombre de una pantalla.
func (b *buzon) ultimoEnlace(t *testing.T) string {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.mensajes) == 0 {
		t.Fatal("no se envió ningún correo")
	}

	cuerpo := b.mensajes[len(b.mensajes)-1].Cuerpo
	encontrado := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(cuerpo)
	if encontrado == nil {
		t.Fatalf("el correo no lleva un enlace con token: %q", cuerpo)
	}
	return encontrado[1]
}

func (b *buzon) ultimoAsunto(t *testing.T) string {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.mensajes) == 0 {
		t.Fatal("no se envió ningún correo")
	}
	return b.mensajes[len(b.mensajes)-1].Asunto
}

func altaNueva(t *testing.T, correo string) api.NuevaCuenta {
	t.Helper()

	email := openapi_types.Email(correo)
	return api.NuevaCuenta{
		Nombre:         "Persona De Prueba",
		Email:          &email,
		Contrasena:     contrasenaBuena,
		AceptaTerminos: true,
	}
}

// registrada da una cuenta ya activa: alta + verificación, que es el camino
// completo de RF-24.
func registrada(t *testing.T, svc *identidad.Servicio, b *buzon, correo string) api.Cuenta {
	t.Helper()

	if err := svc.Registrar(t.Context(), altaNueva(t, correo)); err != nil {
		t.Fatalf("Registrar devolvió error: %v", err)
	}

	cuenta, err := svc.CanjearVerificacion(t.Context(), b.ultimoEnlace(t))
	if err != nil {
		t.Fatalf("CanjearVerificacion devolvió error: %v", err)
	}
	if cuenta.Estado != api.Activa {
		t.Fatalf("la cuenta quedó en %q y se esperaba activa", cuenta.Estado)
	}

	return cuenta
}

// ------------------------------------------------------------------ RF-24 --

func TestElAltaDejaLaCuentaPendienteHastaVerificar(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)

	if err := svc.Registrar(t.Context(), altaNueva(t, correo)); err != nil {
		t.Fatalf("Registrar devolvió error: %v", err)
	}

	// Antes de verificar no se entra, y ese es el punto: si se pudiera, la
	// verificación de RF-19 no sería más que un correo decorativo.
	_, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if !errors.Is(err, identidad.ErrCuentaNoActiva) {
		t.Fatalf("una cuenta sin verificar dejó entrar: %v", err)
	}

	cuenta, err := svc.CanjearVerificacion(t.Context(), buzon.ultimoEnlace(t))
	if err != nil {
		t.Fatalf("CanjearVerificacion devolvió error: %v", err)
	}
	if !cuenta.EmailVerificado || cuenta.Estado != api.Activa {
		t.Fatalf("tras verificar: verificado=%v estado=%q", cuenta.EmailVerificado, cuenta.Estado)
	}
	if cuenta.Tipo != api.TipoCuentaUsuario {
		t.Fatalf("una cuenta registrada por la vía normal debe ser usuario, y es %q", cuenta.Tipo)
	}

	// Un solo uso: el mismo enlace no vale dos veces (RF-19).
	if _, err := svc.CanjearVerificacion(t.Context(), buzon.ultimoEnlace(t)); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el enlace se pudo canjear dos veces: %v", err)
	}
}

// La anti-enumeración de RF-24: registrarse con un correo ya tomado responde
// igual que hacerlo con uno libre. Lo que cambia no es la respuesta, es a quién
// se le avisa.
func TestRegistrarDosVecesNoDelataQueElCorreoExiste(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)

	if err := svc.Registrar(t.Context(), altaNueva(t, correo)); err != nil {
		t.Fatalf("primer Registrar devolvió error: %v", err)
	}
	primerAsunto := buzon.ultimoAsunto(t)

	if err := svc.Registrar(t.Context(), altaNueva(t, correo)); err != nil {
		t.Fatalf("el segundo Registrar delató que el correo existe: %v", err)
	}

	segundoAsunto := buzon.ultimoAsunto(t)
	if segundoAsunto == primerAsunto {
		t.Fatal("el segundo alta mandó el mismo correo que el primero: al titular hay que avisarle del intento")
	}
	if !strings.Contains(strings.ToLower(segundoAsunto), "intent") {
		t.Fatalf("el aviso al titular no se parece a uno: %q", segundoAsunto)
	}
}

func TestElAltaRechazaLoQueNoDependeDeSiLaCuentaExiste(t *testing.T) {
	svc, _ := servicioCuentas(t)

	casos := []struct {
		nombre  string
		ajustar func(*api.NuevaCuenta)
		espera  error
	}{
		{
			nombre:  "sin aceptar los términos",
			ajustar: func(n *api.NuevaCuenta) { n.AceptaTerminos = false },
			espera:  identidad.ErrTerminosNoAceptados,
		},
		{
			nombre:  "contraseña corta",
			ajustar: func(n *api.NuevaCuenta) { n.Contrasena = "corta1" },
			espera:  dominio.ErrContrasenaCorta,
		},
		{
			nombre: "la contraseña contiene el propio correo",
			ajustar: func(n *api.NuevaCuenta) {
				local, _, _ := strings.Cut(string(*n.Email), "@")
				n.Contrasena = local + "-mas-relleno-largo"
			},
			espera: dominio.ErrContrasenaPrevisible,
		},
		{
			nombre:  "sin ningún contacto",
			ajustar: func(n *api.NuevaCuenta) { n.Email = nil },
			espera:  identidad.ErrSinContacto,
		},
	}

	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			nueva := altaNueva(t, destinoUnico(t))
			caso.ajustar(&nueva)

			if err := svc.Registrar(t.Context(), nueva); !errors.Is(err, caso.espera) {
				t.Fatalf("se esperaba %v y salió %v", caso.espera, err)
			}
		})
	}
}

// ------------------------------------------------------------------ RF-22 --

// Cambiar el correo lo deja SIN verificar, y de eso depende algo más que un
// icono: el alcance de una cuenta incluye sus reservas de invitado con ese
// correo (RF-24), así que un correo escrito y no verificado que contara sería
// una forma de heredar las reservas de otra persona.
func TestCambiarElCorreoLoDejaSinVerificar(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))

	nuevo := openapi_types.Email(destinoUnico(t))
	actualizada, err := svc.ActualizarPerfil(t.Context(), cuenta.Id.String(),
		api.ActualizacionCuenta{Email: &nuevo})
	if err != nil {
		t.Fatalf("ActualizarPerfil devolvió error: %v", err)
	}

	if actualizada.EmailVerificado {
		t.Fatal("el correo nuevo quedó verificado sin que nadie abriera nada")
	}
	if actualizada.Email == nil || *actualizada.Email != nuevo {
		t.Fatalf("el correo no se guardó: %v", actualizada.Email)
	}

	// Y sale la verificación del correo nuevo (RF-19).
	if _, err := svc.CanjearVerificacion(t.Context(), buzon.ultimoEnlace(t)); err != nil {
		t.Fatalf("no se pudo verificar el correo nuevo: %v", err)
	}
}

func TestNoSePuedeTomarElCorreoDeOtraCuenta(t *testing.T) {
	svc, buzon := servicioCuentas(t)

	ajena := destinoUnico(t)
	registrada(t, svc, buzon, ajena)
	propia := registrada(t, svc, buzon, destinoUnico(t))

	tomado := openapi_types.Email(ajena)
	_, err := svc.ActualizarPerfil(t.Context(), propia.Id.String(),
		api.ActualizacionCuenta{Email: &tomado})
	if !errors.Is(err, identidad.ErrContactoEnUso) {
		t.Fatalf("se pudo tomar el correo de otra cuenta: %v", err)
	}
}

// ------------------------------------------------------------------ RF-21 --

func TestLasPreferenciasSeReemplazanEnteras(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))
	id := cuenta.Id.String()

	// Sin guardar nada, la lista está vacía: NO es lo mismo que todo
	// deshabilitado, y esa diferencia es la que permite que el valor por
	// defecto del producto alcance a quien nunca tocó esta pantalla.
	lista, err := svc.Preferencias(t.Context(), id)
	if err != nil {
		t.Fatalf("Preferencias devolvió error: %v", err)
	}
	if len(lista.Datos) != 0 {
		t.Fatalf("una cuenta nueva ya traía %d preferencias", len(lista.Datos))
	}

	guardada, err := svc.GuardarPreferencias(t.Context(), id, api.ListaPreferencias{
		Datos: []api.Preferencia{
			{Canal: api.CanalNotificacionEmail, Tipo: "recordatorio", Habilitado: false},
			{Canal: api.CanalNotificacionEmail, Tipo: "confirmacion", Habilitado: true},
		},
	})
	if err != nil {
		t.Fatalf("GuardarPreferencias devolvió error: %v", err)
	}
	if len(guardada.Datos) != 2 {
		t.Fatalf("se guardaron %d preferencias en vez de 2", len(guardada.Datos))
	}

	// Un PUT reemplaza: lo que no viaja vuelve al valor por defecto, y eso
	// significa desaparecer de la lista, no quedarse en false.
	guardada, err = svc.GuardarPreferencias(t.Context(), id, api.ListaPreferencias{
		Datos: []api.Preferencia{
			{Canal: api.CanalNotificacionSms, Tipo: "recordatorio", Habilitado: true},
		},
	})
	if err != nil {
		t.Fatalf("GuardarPreferencias (segunda) devolvió error: %v", err)
	}
	if len(guardada.Datos) != 1 || guardada.Datos[0].Canal != api.CanalNotificacionSms {
		t.Fatalf("el PUT no reemplazó el conjunto: %+v", guardada.Datos)
	}
}

func TestUnTipoDeNotificacionDesconocidoSeRechaza(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))

	_, err := svc.GuardarPreferencias(t.Context(), cuenta.Id.String(), api.ListaPreferencias{
		Datos: []api.Preferencia{
			{Canal: api.CanalNotificacionEmail, Tipo: "inventado", Habilitado: true},
		},
	})
	if !errors.Is(err, identidad.ErrDestinoInvalido) {
		t.Fatalf("se aceptó un tipo de notificación que nadie envía nunca: %v", err)
	}
}

// ------------------------------------------------------------------ RF-25 --

// La baja anonimiza y no borra: los comprobantes de RF-34 referencian la fila.
// Lo que sí desaparece es lo que identifica a una persona, y con ello el correo
// vuelve a estar libre.
func TestLaBajaAnonimizaYLiberaElCorreo(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	cuenta := registrada(t, svc, buzon, correo)

	par, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("IniciarSesion devolvió error: %v", err)
	}

	if err := svc.Eliminar(t.Context(), cuenta.Id.String()); err != nil {
		t.Fatalf("Eliminar devolvió error: %v", err)
	}

	perfil, err := svc.Perfil(t.Context(), cuenta.Id.String())
	if err != nil {
		t.Fatalf("la fila desapareció, y RF-34 la necesita: %v", err)
	}
	if perfil.Estado != api.Eliminada || perfil.Email != nil || perfil.Nombre != nil {
		t.Fatalf("la cuenta no quedó anonimizada: %+v", perfil)
	}

	// Las sesiones caen con la cuenta: una cuenta sin datos pero con sesiones
	// vivas seguiría sirviendo peticiones a nombre de nadie.
	if _, err := svc.Refrescar(t.Context(), par.Refresco, identidad.ContextoSesion{}); !errors.Is(err, identidad.ErrRefrescoInvalido) {
		t.Fatalf("la sesión sobrevivió a la baja: %v", err)
	}

	// Y el correo queda libre: los índices de unicidad excluyen las eliminadas.
	if err := svc.Registrar(t.Context(), altaNueva(t, correo)); err != nil {
		t.Fatalf("el correo quedó quemado tras la baja: %v", err)
	}
	if _, err := svc.CanjearVerificacion(t.Context(), buzon.ultimoEnlace(t)); err != nil {
		t.Fatalf("no se pudo volver a registrar con el mismo correo: %v", err)
	}
}
