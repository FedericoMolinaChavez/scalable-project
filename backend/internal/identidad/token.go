package identidad

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// El token de acceso: quién demostró tener este correo, y hasta cuándo.
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
const prefijoVersion = "rv1"

// Acceso es lo que un token demuestra.
type Acceso struct {
	// Destino es el correo verificado. Es la identidad completa de un invitado:
	// no hay cuenta detrás, solo la prueba de que controla esa dirección.
	Destino string `json:"d"`

	// Expira es cuándo deja de valer. Se comprueba al verificar, y es la única
	// defensa real: no hay lista de revocación porque no hay sesión que
	// revocar (RF-25 gestiona sesiones de cuentas, y un invitado no tiene).
	Expira int64 `json:"x"`
}

// Vencido indica si el token ya no vale por tiempo.
func (a Acceso) Vencido(ahora time.Time) bool {
	return ahora.Unix() >= a.Expira
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

// Emitir crea un token para un destino ya verificado.
func (f *Firmante) Emitir(destino string, ahora time.Time) (string, time.Time, error) {
	expira := ahora.Add(f.vigencia)

	cuerpo, err := json.Marshal(Acceso{Destino: destino, Expira: expira.Unix()})
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
	if acceso.Destino == "" {
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
