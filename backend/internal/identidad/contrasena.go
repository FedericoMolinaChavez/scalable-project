package identidad

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id para las contraseñas (RNF-09), y no SHA-256 como para los códigos.
//
// La diferencia no es de gusto. Un código de RF-02 son seis dígitos que viven
// cinco minutos y aguantan tres intentos: su defensa es el tiempo y el
// contador, y un hash lento no compraría nada. Una contraseña la elige una
// persona, vive años y se prueba OFFLINE si alguien se lleva la tabla, donde no
// hay contador que valga. Ahí lo único que encarece el ataque es que cada
// intento cueste memoria y tiempo, que es exactamente lo que hace Argon2id.
//
// Los parámetros son los que recomienda OWASP para argon2id: 19 MiB, dos
// pasadas y un hilo. La memoria es el parámetro que importa —es lo que un
// atacante no puede paralelizar barato en GPU— y 19 MiB por verificación es
// asumible en un servicio que autentica, no en uno que sirve el 90% del tráfico.
//
// Van GUARDADOS dentro del hash, en el formato PHC estándar. Es lo que permite
// subirlos más adelante sin invalidar las contraseñas existentes: cada fila se
// verifica con los parámetros con los que se escribió, y se puede rehacer al
// siguiente inicio de sesión correcto.
const (
	argonMemoria     uint32 = 19 * 1024 // KiB
	argonIteraciones uint32 = 2
	argonHilos       uint8  = 1
	argonLongitud    uint32 = 32
	argonSal         uint32 = 16
)

// ErrHashIlegible es un password_hash que no tiene el formato esperado.
//
// No se trata como "contraseña incorrecta". Una fila corrupta es un fallo del
// sistema, y devolverla como credencial equivocada la escondería: quien no
// puede entrar reintentaría para siempre sin que nada apareciera en los
// registros.
var ErrHashIlegible = errors.New("el hash de la contraseña no se puede interpretar")

// HashContrasena deriva el hash que se guarda en plataforma.cuenta.
func HashContrasena(contrasena string) (string, error) {
	sal := make([]byte, argonSal)
	if _, err := rand.Read(sal); err != nil {
		return "", fmt.Errorf("no se pudo generar la sal: %w", err)
	}

	suma := argon2.IDKey(
		[]byte(contrasena), sal, argonIteraciones, argonMemoria, argonHilos, argonLongitud)

	// Formato PHC, el mismo que usan las bibliotecas de otros lenguajes. Que
	// sea estándar no es cosmética: si algún día hay que verificar estas
	// contraseñas desde fuera de Go, el formato ya está descrito.
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemoria, argonIteraciones, argonHilos,
		base64.RawStdEncoding.EncodeToString(sal),
		base64.RawStdEncoding.EncodeToString(suma),
	), nil
}

// VerificarContrasena comprueba una contraseña contra su hash.
//
// Devuelve (false, nil) cuando simplemente no coincide, y error solo cuando el
// hash guardado no se puede interpretar.
func VerificarContrasena(hash, contrasena string) (bool, error) {
	partes := strings.Split(hash, "$")
	if len(partes) != 6 || partes[1] != "argon2id" {
		return false, ErrHashIlegible
	}

	var version int
	if _, err := fmt.Sscanf(partes[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, ErrHashIlegible
	}

	var (
		memoria     uint32
		iteraciones uint32
		hilos       uint8
	)
	if _, err := fmt.Sscanf(partes[3], "m=%d,t=%d,p=%d", &memoria, &iteraciones, &hilos); err != nil {
		return false, ErrHashIlegible
	}

	sal, err := base64.RawStdEncoding.DecodeString(partes[4])
	if err != nil {
		return false, ErrHashIlegible
	}
	esperado, err := base64.RawStdEncoding.DecodeString(partes[5])
	if err != nil {
		return false, ErrHashIlegible
	}

	// Con los parámetros de la FILA, no con las constantes de arriba. Es lo que
	// permite subir el coste sin invalidar lo ya guardado.
	calculado := argon2.IDKey(
		[]byte(contrasena), sal, iteraciones, memoria, hilos, uint32(len(esperado)))

	// subtle.ConstantTimeCompare y no bytes.Equal: comparar cortando en el
	// primer byte distinto filtra por tiempo cuántos se acertaron.
	return subtle.ConstantTimeCompare(calculado, esperado) == 1, nil
}
