package dominio

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// La política de contraseñas de RF-24 y RF-18.
//
// Ninguno de los dos diagramas fija los parámetros: dicen "cumple la política
// de seguridad (longitud, complejidad)" y dejan la política a quien la
// implemente. La que hay aquí sigue a NIST SP 800-63B, y eso significa algo
// distinto de lo que suele entenderse por "compleja":
//
// Se exige LONGITUD y se rechaza lo ADIVINABLE, pero no se exigen clases de
// caracteres. La regla de "una mayúscula, un número y un símbolo" produce
// contraseñas peores, no mejores: empuja a la misma transformación previsible
// —`Contrasena1!`— sobre una palabra corta, que un diccionario con reglas
// prueba en segundos, mientras que castiga una frase larga y realmente difícil.
// Lo que sí se comprueba es que no contenga el correo ni el nombre de quien la
// elige, porque eso es lo primero que prueba quien ataca una cuenta concreta.
//
// El máximo existe por una razón mecánica y no de seguridad: Argon2id trabaja
// sobre la entrada completa, así que sin tope una contraseña de un megabyte es
// una forma barata de consumir CPU y memoria del servicio de identidad.
const (
	LongitudMinimaContrasena = 12
	LongitudMaximaContrasena = 128
)

var (
	ErrContrasenaCorta = errors.New(
		"la contraseña debe tener al menos 12 caracteres")

	ErrContrasenaLarga = errors.New(
		"la contraseña no puede pasar de 128 caracteres")

	// ErrContrasenaPrevisible cubre los tres casos de "esto se adivina": está
	// en la lista de las más usadas, repite un solo carácter, o contiene el
	// correo o el nombre de la propia cuenta. Es un error y no tres porque lo
	// que la persona tiene que hacer es el mismo en los tres: elegir otra.
	ErrContrasenaPrevisible = errors.New(
		"esa contraseña es demasiado fácil de adivinar; elige otra que no contenga tu nombre ni tu correo")
)

// masUsadas son las que aparecen primero en cualquier lista de credenciales
// filtradas. No pretende ser exhaustiva —una lista completa vive en un archivo
// de decenas de miles de líneas y se consulta con un filtro de Bloom— sino
// cortar el caso que de verdad ocurre: alguien escribiendo lo primero que se le
// pasa por la cabeza para salir del formulario.
var masUsadas = map[string]bool{
	"contrasena":    true,
	"contraseña":    true,
	"contrasena123": true,
	"password":      true,
	"password123":   true,
	"passw0rd":      true,
	"123456789012":  true,
	"1234567890123": true,
	"qwertyuiopas":  true,
	"administrador": true,
	"iloveyou1234":  true,
	"reservas1234":  true,
	"bienvenido12":  true,
	"secreto12345":  true,
	"letmein12345":  true,
	"cambiame1234":  true,
}

// ValidarContrasena aplica la política. `identificadores` son los datos de la
// propia cuenta que la contraseña no debe contener: el correo y el nombre.
//
// Se pasan como parámetro en vez de leerse aquí porque esta función no conoce
// la cuenta, y no debe: es una regla sobre un texto, y así se puede aplicar
// tanto en el alta —donde la cuenta todavía no existe— como en un cambio.
func ValidarContrasena(contrasena string, identificadores ...string) error {
	// Se cuenta en runas y no en bytes. Con len() una frase en español de doce
	// caracteres pasaría el mínimo por sus tildes, y una de once también: el
	// requisito quedaría dependiendo del idioma de quien lo escribe.
	if utf8.RuneCountInString(contrasena) < LongitudMinimaContrasena {
		return ErrContrasenaCorta
	}
	if utf8.RuneCountInString(contrasena) > LongitudMaximaContrasena {
		return ErrContrasenaLarga
	}

	minuscula := strings.ToLower(contrasena)

	if masUsadas[minuscula] {
		return ErrContrasenaPrevisible
	}

	if unSoloCaracter(contrasena) {
		return ErrContrasenaPrevisible
	}

	for _, id := range identificadores {
		id = strings.ToLower(strings.TrimSpace(id))

		// De un correo se compara la parte local: quien usa `ana@ejemplo.com`
		// elige `ana2024`, no `ana@ejemplo.com2024`.
		if local, _, hay := strings.Cut(id, "@"); hay {
			id = local
		}

		// El umbral evita que un nombre de dos letras convierta en previsible
		// cualquier contraseña que las contenga en algún sitio.
		if len(id) >= 4 && strings.Contains(minuscula, id) {
			return ErrContrasenaPrevisible
		}
	}

	return nil
}

// unSoloCaracter detecta `aaaaaaaaaaaa` y sus variantes, que pasan cualquier
// mínimo de longitud y no aportan ninguna entropía.
func unSoloCaracter(s string) bool {
	primero, _ := utf8.DecodeRuneInString(s)
	for _, r := range s {
		if r != primero {
			return false
		}
	}
	return true
}
