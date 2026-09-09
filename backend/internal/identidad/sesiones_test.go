package identidad_test

import (
	"errors"
	"testing"
	"time"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
)

// ------------------------------------------------------------------ RF-12 --

func TestIniciarSesionDevuelveUnAccesoConLaCuentaDentro(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	cuenta := registrada(t, svc, buzon, correo)

	par, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena,
		identidad.ContextoSesion{Dispositivo: "prueba", IP: "203.0.113.7"})
	if err != nil {
		t.Fatalf("IniciarSesion devolvió error: %v", err)
	}

	firmante := identidad.NuevoFirmante([]byte(secreto), 15*time.Minute)
	acceso, err := firmante.Verificar(par.Acceso, time.Now())
	if err != nil {
		t.Fatalf("el acceso emitido no se puede verificar: %v", err)
	}

	if acceso.EsInvitado() {
		t.Fatal("el token de una cuenta salió como de invitado")
	}
	if acceso.Cuenta != cuenta.Id.String() {
		t.Fatalf("el token acredita la cuenta %q y se esperaba %q", acceso.Cuenta, cuenta.Id)
	}
	if acceso.Tipo != identidad.TipoUsuario {
		t.Fatalf("el tipo dentro del token es %q", acceso.Tipo)
	}
	if acceso.Tenant != "" {
		t.Fatalf("un usuario no administra ningún tenant y el token trae %q", acceso.Tenant)
	}
	if acceso.Sesion == "" {
		t.Fatal("el token no dice de qué sesión salió, y RF-25 lo necesita para marcar la actual")
	}
	if acceso.Destino != correo {
		t.Fatalf("el correo verificado no viajó en el token: %q", acceso.Destino)
	}
}

// El correo equivocado y la contraseña equivocada responden lo mismo. Es la
// anti-enumeración de RF-12 A12, y separarlas convertiría el formulario de
// entrada en un comprobador de direcciones registradas.
func TestUnCorreoDesconocidoYUnaContrasenaMalaSonElMismoError(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	registrada(t, svc, buzon, correo)

	_, errDesconocido := svc.IniciarSesion(
		t.Context(), destinoUnico(t), contrasenaBuena, identidad.ContextoSesion{})
	_, errMala := svc.IniciarSesion(
		t.Context(), correo, "otra-cosa-larguisima", identidad.ContextoSesion{})

	if !errors.Is(errDesconocido, identidad.ErrCredencialesInvalidas) {
		t.Fatalf("correo desconocido dio %v", errDesconocido)
	}
	if !errors.Is(errMala, identidad.ErrCredencialesInvalidas) {
		t.Fatalf("contraseña mala dio %v", errMala)
	}
	if errDesconocido.Error() != errMala.Error() {
		t.Fatalf("los dos errores se distinguen desde fuera:\n  %v\n  %v", errDesconocido, errMala)
	}
}

// RF-12 A3/A4: fallos CONSECUTIVOS. La palabra importa —un acierto en medio
// pone el contador a cero— y sin esa parte, cinco entradas legítimas repartidas
// en el tiempo acabarían bloqueando la cuenta.
func TestLosFallosConsecutivosBloqueanYUnAciertoLosBorra(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	registrada(t, svc, buzon, correo)

	// Dos fallos, uno menos que el máximo de esta configuración.
	for i := range 2 {
		if _, err := svc.IniciarSesion(
			t.Context(), correo, "mal-mal-mal-mal", identidad.ContextoSesion{},
		); !errors.Is(err, identidad.ErrCredencialesInvalidas) {
			t.Fatalf("fallo %d dio %v", i, err)
		}
	}

	// Un acierto borra el contador.
	if _, err := svc.IniciarSesion(
		t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("no se pudo entrar con la contraseña correcta: %v", err)
	}

	// Y ahora hacen falta los tres fallos completos para bloquear.
	for i := range 3 {
		if _, err := svc.IniciarSesion(
			t.Context(), correo, "mal-mal-mal-mal", identidad.ContextoSesion{},
		); !errors.Is(err, identidad.ErrCredencialesInvalidas) {
			t.Fatalf("tras el acierto, el fallo %d dio %v", i, err)
		}
	}

	// El bloqueo se distingue de las credenciales malas: es un 429, no un 401,
	// porque la contraseña puede estar perfectamente bien.
	if _, err := svc.IniciarSesion(
		t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrCuentaBloqueada) {
		t.Fatalf("la cuenta no quedó bloqueada tras los fallos: %v", err)
	}
}

func TestElMagicLinkAbreSesionYNoSirveDosVeces(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	registrada(t, svc, buzon, correo)

	if err := svc.SolicitarEnlaceEntrada(t.Context(), correo); err != nil {
		t.Fatalf("SolicitarEnlaceEntrada devolvió error: %v", err)
	}
	enlace := buzon.ultimoEnlace(t)

	if _, err := svc.CanjearEnlaceEntrada(
		t.Context(), enlace, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("CanjearEnlaceEntrada devolvió error: %v", err)
	}

	if _, err := svc.CanjearEnlaceEntrada(
		t.Context(), enlace, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el mismo enlace abrió dos sesiones: %v", err)
	}
}

// Pedir un enlace para un correo sin cuenta responde igual que para uno con
// cuenta, y no manda nada (RF-12 A12).
func TestPedirUnEnlaceParaUnCorreoSinCuentaNoDelataNada(t *testing.T) {
	svc, buzon := servicioCuentas(t)

	if err := svc.SolicitarEnlaceEntrada(t.Context(), destinoUnico(t)); err != nil {
		t.Fatalf("delató que el correo no existe: %v", err)
	}
	if buzon.enviados() != 0 {
		t.Fatal("se mandó un correo a una dirección sin cuenta")
	}
}

// El refresco ROTA: el valor presentado deja de valer en cuanto se usa. Es lo
// que hace que un refresco copiado se note, en vez de quedar utilizable en
// paralelo hasta que caduque la sesión.
func TestElRefrescoRotaYElAnteriorMuere(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	registrada(t, svc, buzon, correo)

	par, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("IniciarSesion devolvió error: %v", err)
	}

	renovado, err := svc.Refrescar(t.Context(), par.Refresco, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("Refrescar devolvió error: %v", err)
	}
	if renovado.Refresco == par.Refresco {
		t.Fatal("el refresco no rotó: se devolvió el mismo valor")
	}

	if _, err := svc.Refrescar(
		t.Context(), par.Refresco, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrRefrescoInvalido) {
		t.Fatalf("el refresco anterior siguió sirviendo: %v", err)
	}
}

// ------------------------------------------------------------------ RF-25 --

func TestLasSesionesSeListanYLaPropiaSeDistingue(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	cuenta := registrada(t, svc, buzon, correo)
	id := cuenta.Id.String()

	primera, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena,
		identidad.ContextoSesion{Dispositivo: "portátil", IP: "203.0.113.1"})
	if err != nil {
		t.Fatalf("primera sesión: %v", err)
	}
	if _, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena,
		identidad.ContextoSesion{Dispositivo: "móvil", IP: "203.0.113.2"}); err != nil {
		t.Fatalf("segunda sesión: %v", err)
	}

	firmante := identidad.NuevoFirmante([]byte(secreto), 15*time.Minute)
	acceso, err := firmante.Verificar(primera.Acceso, time.Now())
	if err != nil {
		t.Fatalf("no se pudo leer el acceso: %v", err)
	}

	lista, err := svc.ListarSesiones(t.Context(), id, acceso.Sesion)
	if err != nil {
		t.Fatalf("ListarSesiones devolvió error: %v", err)
	}
	if len(lista.Datos) != 2 {
		t.Fatalf("se listaron %d sesiones y hay 2", len(lista.Datos))
	}

	var actuales int
	for _, sesion := range lista.Datos {
		if sesion.Actual {
			actuales++
			if sesion.Id.String() != acceso.Sesion {
				t.Fatalf("se marcó como actual una sesión que no lo es: %s", sesion.Id)
			}
		}
	}
	if actuales != 1 {
		t.Fatalf("hay %d sesiones marcadas como actuales; sin exactamente una, cerrar la otra es adivinar", actuales)
	}
}

func TestRevocarUnaSesionCortaSoloEsa(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	cuenta := registrada(t, svc, buzon, correo)
	id := cuenta.Id.String()

	primera, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("primera sesión: %v", err)
	}
	segunda, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("segunda sesión: %v", err)
	}

	firmante := identidad.NuevoFirmante([]byte(secreto), 15*time.Minute)
	accesoPrimera, err := firmante.Verificar(primera.Acceso, time.Now())
	if err != nil {
		t.Fatalf("no se pudo leer el acceso: %v", err)
	}

	if err := svc.RevocarSesion(t.Context(), id, accesoPrimera.Sesion); err != nil {
		t.Fatalf("RevocarSesion devolvió error: %v", err)
	}

	if _, err := svc.Refrescar(
		t.Context(), primera.Refresco, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrRefrescoInvalido) {
		t.Fatalf("la sesión revocada siguió refrescando: %v", err)
	}
	if _, err := svc.Refrescar(
		t.Context(), segunda.Refresco, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("revocar una sesión se llevó por delante la otra: %v", err)
	}
}

// ------------------------------------------------------------------ RF-18 --

// Cambiar la contraseña invalida las demás sesiones. Es el sentido del gesto:
// se cambia justo cuando se sospecha que alguien más la tiene.
func TestCambiarLaContrasenaCortaLasDemasSesiones(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	cuenta := registrada(t, svc, buzon, correo)
	id := cuenta.Id.String()

	desdeDondeCambia, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("sesión propia: %v", err)
	}
	otra, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("otra sesión: %v", err)
	}

	firmante := identidad.NuevoFirmante([]byte(secreto), 15*time.Minute)
	acceso, err := firmante.Verificar(desdeDondeCambia.Acceso, time.Now())
	if err != nil {
		t.Fatalf("no se pudo leer el acceso: %v", err)
	}

	const nueva = "otra-frase-larga-distinta"
	if err := svc.CambiarContrasena(
		t.Context(), id, acceso.Sesion, contrasenaBuena, nueva,
	); err != nil {
		t.Fatalf("CambiarContrasena devolvió error: %v", err)
	}

	if _, err := svc.Refrescar(
		t.Context(), otra.Refresco, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrRefrescoInvalido) {
		t.Fatalf("la otra sesión sobrevivió al cambio de contraseña: %v", err)
	}

	// La sesión desde la que se cambia SÍ sobrevive: quien la usa ya demostró
	// saber la contraseña anterior, y echarlo sería castigar el gesto correcto.
	if _, err := svc.Refrescar(
		t.Context(), desdeDondeCambia.Refresco, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("la sesión que cambió la contraseña se cortó a sí misma: %v", err)
	}

	// Y la contraseña nueva es la que vale.
	if _, err := svc.IniciarSesion(
		t.Context(), correo, nueva, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("no se pudo entrar con la contraseña nueva: %v", err)
	}
	if _, err := svc.IniciarSesion(
		t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrCredencialesInvalidas) {
		t.Fatalf("la contraseña vieja siguió sirviendo: %v", err)
	}
}

// La contraseña ACTUAL es obligatoria aunque haya sesión: un token robado no
// debe bastar para quedarse con la cuenta.
func TestCambiarLaContrasenaExigeLaActual(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))

	err := svc.CambiarContrasena(
		t.Context(), cuenta.Id.String(), "", "la-que-no-es-pero-larga", "otra-frase-larga-distinta")
	if !errors.Is(err, identidad.ErrCredencialesInvalidas) {
		t.Fatalf("se cambió la contraseña sin saber la anterior: %v", err)
	}
}

func TestRestablecerConElEnlaceRevocaTodo(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	correo := destinoUnico(t)
	registrada(t, svc, buzon, correo)

	viva, err := svc.IniciarSesion(t.Context(), correo, contrasenaBuena, identidad.ContextoSesion{})
	if err != nil {
		t.Fatalf("IniciarSesion devolvió error: %v", err)
	}

	if err := svc.SolicitarRecuperacion(t.Context(), correo); err != nil {
		t.Fatalf("SolicitarRecuperacion devolvió error: %v", err)
	}
	enlace := buzon.ultimoEnlace(t)

	const nueva = "otra-frase-larga-distinta"
	if err := svc.Restablecer(t.Context(), enlace, nueva); err != nil {
		t.Fatalf("Restablecer devolvió error: %v", err)
	}

	// Aquí SÍ caen todas, incluida cualquiera que estuviera abierta: quien
	// restablece es quien no podía entrar, así que no hay ninguna sesión suya
	// que valga la pena conservar.
	if _, err := svc.Refrescar(
		t.Context(), viva.Refresco, identidad.ContextoSesion{},
	); !errors.Is(err, identidad.ErrRefrescoInvalido) {
		t.Fatalf("una sesión sobrevivió al restablecimiento: %v", err)
	}

	if _, err := svc.IniciarSesion(
		t.Context(), correo, nueva, identidad.ContextoSesion{},
	); err != nil {
		t.Fatalf("no se pudo entrar con la contraseña restablecida: %v", err)
	}

	// El enlace es de un solo uso.
	if err := svc.Restablecer(
		t.Context(), enlace, "y-otra-frase-mas-larga",
	); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el enlace de recuperación sirvió dos veces: %v", err)
	}
}

func TestPedirRecuperacionParaUnCorreoSinCuentaNoMandaNada(t *testing.T) {
	svc, buzon := servicioCuentas(t)

	if err := svc.SolicitarRecuperacion(t.Context(), destinoUnico(t)); err != nil {
		t.Fatalf("delató que el correo no existe: %v", err)
	}
	if buzon.enviados() != 0 {
		t.Fatal("se mandó un enlace a una dirección sin cuenta")
	}
}
