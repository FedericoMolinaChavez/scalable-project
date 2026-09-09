package identidad

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// El token de acceso: qué demostró quien lo presenta, y hasta cuándo.
//
// Formato propio y no JWT, por dos razones concretas:
//
// El algoritmo no se negocia. Un JWT lleva el algoritmo DENTRO del token, y esa
// negociación es una familia entera de vulnerabilidades: `alg: none`, o
// verificar con HMAC un token que decía ser RS256 usando la clave pública como
// secreto. Aquí solo existe un algoritmo, va en el prefijo de versión, y un
// token que no empiece por ese prefijo se rechaza antes de mirar nada más. No
// hay nada que confundir porque no hay nada que elegir.
//
// Los siete componentes de ARQ-01 viven en un solo módulo Go, así que un
// formato propio no obliga a nadie externo a implementarlo: se importa este
// paquete y ya está. Eso deja de ser cierto en cuanto el Gateway tenga que
// validar tokens por su cuenta, y ese es exactamente el momento de cambiar a
// firma asimétrica estándar.
//
// Lo que NO lleva dentro: nada que no sea necesario para decidir el alcance. Un
// token es un dato que viaja por la red y se guarda en un navegador; meterle el
// nombre de la persona o el identificador de sus reservas sería regalar
// información a cualquiera que lo intercepte, y el contenido de este formato no
// va cifrado, solo firmado.
//
// La versión es `rv2` y no `rv1` porque el contenido cambió al aparecer las
// cuentas (RF-12): antes un token solo podía significar "controlo este correo",
// ahora significa además "soy esta cuenta, de este tipo" o "soy un agente
// actuando por esta cuenta". Subir el prefijo es justamente para lo que existe:
// un token del formato anterior se rechaza en la primera comparación en vez de
// interpretarse con las reglas nuevas y acabar con un alcance que nadie emitió.
// El coste de invalidar los que hubiera en circulación es nulo: viven minutos.
const prefijoVersion = "rv2"

// Los tres tipos de cuenta de RF-23. Son constantes de este paquete y no del
// generado desde el contrato porque el token es anterior al contrato: se emite
// y se verifica aunque ninguna ruta HTTP esté montada.
const (
	TipoUsuario    = "usuario"
	TipoAdmin      = "administrador"
	TipoSuperAdmin = "super_admin"
)

// Acceso es lo que un token demuestra.
//
// Las claves JSON son de una letra a propósito: el token viaja en cada
// petición, va en base64 y no se comprime. `cuenta_impersonada_id` ocuparía más
// que el UUID que transporta.
type Acceso struct {
	// Destino es el correo verificado. En un token de invitado es la identidad
	// COMPLETA: no hay cuenta detrás, solo la prueba de que controla esa
	// dirección. En uno de cuenta es informativo y el alcance lo da Cuenta.
	Destino string `json:"d,omitempty"`

	// Expira es cuándo deja de valer. Es la única defensa real de este token:
	// no se comprueba contra la base al verificarlo —es lo que le permite
	// sostener la ruta de lectura de RNF-03— así que revocar una sesión corta
	// el refresco al instante y este al vencer.
	Expira int64 `json:"x"`

	// Cuenta es el identificador de la cuenta. Vacío = invitado (RF-02).
	Cuenta string `json:"c,omitempty"`

	// Tipo es de dónde sale el alcance de RF-23: usuario, administrador o
	// super_admin. Va DENTRO del token, firmado, y no se relee de la base en
	// cada petición. La consecuencia hay que aceptarla: degradar a alguien de
	// administrador a usuario no le quita el alcance hasta que su token venza.
	// Es el mismo compromiso que la revocación, y se acorta por el mismo sitio.
	Tipo string `json:"t,omitempty"`

	// Tenant es el negocio que administra, y solo lo lleva un administrador.
	// Es lo que hace que la cabecera X-Tenant-Id deje de decidir nada para
	// quien tiene cuenta: un administrador opera sobre SU tenant, no sobre el
	// que escriba en una cabecera.
	Tenant string `json:"n,omitempty"`

	// Sesion es la sesión que lo emitió (RF-25). No se comprueba al verificar,
	// por lo dicho en Expira; sirve para que un refresco sepa a qué fila
	// pertenece y para poder registrar desde qué sesión se hizo algo.
	Sesion string `json:"s,omitempty"`

	// Agente, cuando lo hay, es quien actúa EN NOMBRE de Cuenta (RF-13). Su
	// presencia no amplía nada: el alcance sigue siendo el de la cuenta
	// impersonada, y además queda acotado por Alcance.
	Agente string `json:"g,omitempty"`

	// Alcance son las acciones concretas que un token de agente autoriza,
	// ya intersecadas al emitirlo (RF-13/RF-23). Solo tiene sentido con Agente:
	// un token de persona no lleva lista porque su alcance es el de su tipo,
	// no una enumeración.
	Alcance []string `json:"a,omitempty"`
}

// Vencido indica si el token ya no vale por tiempo.
func (a Acceso) Vencido(ahora time.Time) bool {
	return ahora.Unix() >= a.Expira
}

// EsInvitado distingue al que reservó sin cuenta (RF-02) del que tiene una.
//
// Importa en cada consulta acotada: el alcance de un invitado es el correo con
// el que reservó, y el de una cuenta es la cuenta. Confundirlos devolvería a un
// usuario registrado las reservas de invitado de cualquiera que use su misma
// dirección.
func (a Acceso) EsInvitado() bool {
	return a.Cuenta == ""
}

// PorAgente indica si quien pide no es el titular sino un agente en su nombre.
func (a Acceso) PorAgente() bool {
	return a.Agente != ""
}

// Permite comprueba que la acción esté dentro del alcance del token.
//
// Solo acota a los agentes. Una persona no lleva lista de acciones: su alcance
// sale del tipo de cuenta (RF-23) y lo aplica quien ejecuta la acción, no el
// token. Un agente sí, porque RF-13 emite su token para un propósito concreto
// y usarlo para otro es justamente lo que hay que impedir.
func (a Acceso) Permite(accion string) bool {
	if !a.PorAgente() {
		return true
	}
	return slices.Contains(a.Alcance, accion)
}

var (
	// ErrTokenInvalido cubre TODAS las formas de que un token no valga: mal
	// formado, firma que no cuadra, versión desconocida, contenido ilegible.
	// Es deliberadamente uno solo. Distinguirlos en la respuesta le diría a
	// quien está probando tokens en qué se está equivocando, que es justo la
	// información que necesita para acertar.
	ErrTokenInvalido = errors.New("el token de acceso no es válido")

	// ErrTokenVencido sí se distingue, y no es una contradicción: caducar no es
	// un fallo del que lo presenta, es el funcionamiento normal, y la interfaz
	// tiene que poder decir "vuelve a identificarte" en vez de "no eres tú".
	ErrTokenVencido = errors.New("el token de acceso caducó")
)

// Firmante emite y verifica tokens de acceso.
type Firmante struct {
	secreto  []byte
	vigencia time.Duration
}

func NuevoFirmante(secreto []byte, vigencia time.Duration) *Firmante {
	return &Firmante{secreto: secreto, vigencia: vigencia}
}

// Vigencia es cuánto vive un token recién emitido. La expone porque es también
// la ventana de revocación de RF-25, y quien decide esa política necesita poder
// leerla sin duplicar el número.
func (f *Firmante) Vigencia() time.Duration {
	return f.vigencia
}

// Emitir crea un token para un destino de invitado ya verificado (RF-02).
func (f *Firmante) Emitir(destino string, ahora time.Time) (string, time.Time, error) {
	return f.EmitirAcceso(Acceso{Destino: destino}, ahora)
}

// EmitirAcceso firma un acceso completo: cuenta, tipo, tenant, agente y alcance.
//
// La caducidad la pone SIEMPRE esta función y nunca quien llama. Un emisor que
// aceptara un Expira de fuera acabaría, tarde o temprano, firmando el que
// venga en una petición.
func (f *Firmante) EmitirAcceso(acceso Acceso, ahora time.Time) (string, time.Time, error) {
	expira := ahora.Add(f.vigencia)
	acceso.Expira = expira.Unix()

	cuerpo, err := json.Marshal(acceso)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("no se pudo serializar el token: %w", err)
	}

	// base64url sin relleno: el token viaja en una cabecera Authorization y en
	// el almacenamiento del navegador, y el `=` del relleno obliga a escapar
	// en sitios donde nadie se acuerda de hacerlo.
	carga := base64.RawURLEncoding.EncodeToString(cuerpo)
	firmado := prefijoVersion + "." + carga

	return firmado + "." + f.firma(firmado), expira, nil
}

// Verificar comprueba la firma y la vigencia, en ese orden.
//
// El orden importa: comprobar la caducidad antes que la firma le permitiría a
// cualquiera distinguir un token caducado de uno falso sin tener la clave,
// simplemente cambiando la fecha y viendo qué error sale.
func (f *Firmante) Verificar(token string, ahora time.Time) (Acceso, error) {
	version, resto, hay := strings.Cut(token, ".")
	if !hay || version != prefijoVersion {
		return Acceso{}, ErrTokenInvalido
	}

	carga, firma, hay := strings.Cut(resto, ".")
	if !hay {
		return Acceso{}, ErrTokenInvalido
	}

	// hmac.Equal y no ==: la comparación de cadenas de Go corta en el primer
	// byte distinto, y ese tiempo distinto filtra, byte a byte, cuál era la
	// firma correcta. hmac.Equal tarda lo mismo acierte o no.
	if !hmac.Equal([]byte(firma), []byte(f.firma(version+"."+carga))) {
		return Acceso{}, ErrTokenInvalido
	}

	crudo, err := base64.RawURLEncoding.DecodeString(carga)
	if err != nil {
		return Acceso{}, ErrTokenInvalido
	}

	var acceso Acceso
	if err := json.Unmarshal(crudo, &acceso); err != nil {
		return Acceso{}, ErrTokenInvalido
	}

	// Un token tiene que acreditar a ALGUIEN: un correo de invitado o una
	// cuenta. Uno sin ninguna de las dos cosas pasa la firma —la firma solo
	// dice que lo emitimos nosotros— y llegaría al alcance como un sujeto
	// vacío, que es la forma más silenciosa de no acotar nada.
	if acceso.Destino == "" && acceso.Cuenta == "" {
		return Acceso{}, ErrTokenInvalido
	}

	// Un agente sin alcance no autoriza nada, y ese caso ya lo impide el CHECK
	// de token_agente al emitirlo. Comprobarlo también aquí cubre el token que
	// se firmó por otro camino.
	if acceso.PorAgente() && len(acceso.Alcance) == 0 {
		return Acceso{}, ErrTokenInvalido
	}

	if acceso.Vencido(ahora) {
		return Acceso{}, ErrTokenVencido
	}

	return acceso, nil
}

func (f *Firmante) firma(mensaje string) string {
	mac := hmac.New(sha256.New, f.secreto)
	mac.Write([]byte(mensaje))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
