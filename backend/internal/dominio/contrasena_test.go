package dominio_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
)

// La política de RF-24 y RF-18, con lo que acepta y lo que no.
//
// Que exista esta prueba es la mitad del punto: "cumple la política de
// seguridad" es una frase que cada implementación entiende a su manera, y aquí
// queda fijado qué significa exactamente en este sistema.
func TestLaPoliticaDeContrasenas(t *testing.T) {
	casos := []struct {
		nombre          string
		contrasena      string
		identificadores []string
		espera          error
	}{
		{
			nombre:     "una frase larga pasa, sin exigirle mayúsculas ni símbolos",
			contrasena: "caballo grapa correcta",
		},
		{
			nombre:     "once caracteres no llegan",
			contrasena: "once-carac",
			espera:     dominio.ErrContrasenaCorta,
		},
		{
			nombre:     "doce sí",
			contrasena: "doce-caract1",
		},
		{
			// Se cuenta en runas, no en bytes. Con len() esta pasaría por sus
			// tildes aun teniendo once caracteres, y el mínimo acabaría
			// dependiendo del idioma de quien escribe.
			nombre:     "once caracteres con tildes siguen siendo once",
			contrasena: "ñáéíóúñáéíó",
			espera:     dominio.ErrContrasenaCorta,
		},
		{
			nombre:     "más de 128 se rechaza, y no por seguridad sino por coste",
			contrasena: strings.Repeat("a", 129),
			espera:     dominio.ErrContrasenaLarga,
		},
		{
			nombre:     "un solo carácter repetido no aporta entropía",
			contrasena: strings.Repeat("a", 20),
			espera:     dominio.ErrContrasenaPrevisible,
		},
		{
			nombre:     "las más usadas se rechazan",
			contrasena: "contrasena123",
			espera:     dominio.ErrContrasenaPrevisible,
		},
		{
			nombre:          "no puede contener el propio correo",
			contrasena:      "anabelen-y-mas-relleno",
			identificadores: []string{"anabelen@ejemplo.test"},
			espera:          dominio.ErrContrasenaPrevisible,
		},
		{
			nombre:          "ni el propio nombre",
			contrasena:      "mi-clave-fernanda-2028",
			identificadores: []string{"otra@ejemplo.test", "Fernanda"},
			espera:          dominio.ErrContrasenaPrevisible,
		},
		{
			// El umbral de cuatro letras evita que un nombre cortísimo declare
			// previsible cualquier contraseña que lo contenga por casualidad.
			nombre:          "un identificador de tres letras no descalifica nada",
			contrasena:      "una-frase-larga-y-buena",
			identificadores: []string{"ana@ejemplo.test"},
		},
	}

	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			err := dominio.ValidarContrasena(caso.contrasena, caso.identificadores...)
			if !errors.Is(err, caso.espera) {
				t.Fatalf("se esperaba %v y salió %v", caso.espera, err)
			}
		})
	}
}
