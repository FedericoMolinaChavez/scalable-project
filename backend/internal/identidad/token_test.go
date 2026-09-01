package identidad_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
)

// El token no toca la base: es firma y tiempo. Estas pruebas corren siempre,
// con infraestructura o sin ella.

const secreto = "secreto-de-prueba"

func firmante(t *testing.T) *identidad.Firmante {
	t.Helper()
	return identidad.NuevoFirmante([]byte(secreto), 15*time.Minute)
}

func TestEmitirYVerificarConservaElDestino(t *testing.T) {
	f := firmante(t)
	ahora := time.Now()

	token, expira, err := f.Emitir("ana@ejemplo.test", ahora)
	if err != nil {
		t.Fatalf("Emitir devolvió error: %v", err)
	}
	if !expira.After(ahora) {
		t.Fatalf("el token nace caducado: expira %s y son las %s", expira, ahora)
	}

	acceso, err := f.Verificar(token, ahora)
	if err != nil {
		t.Fatalf("Verificar devolvió error: %v", err)
	}
	if acceso.Destino != "ana@ejemplo.test" {
		t.Fatalf("el destino cambió: %q", acceso.Destino)
	}
}

// Lo que un token NO puede llevar dentro es tan importante como lo que lleva.
// El contenido va firmado, no cifrado: cualquiera que intercepte el token puede
// leerlo. Si alguna vez alguien añade aquí un nombre o un identificador de
// reserva, esta prueba lo dice.
func TestElTokenNoLlevaMasQueDestinoYCaducidad(t *testing.T) {
	f := firmante(t)

	token, _, err := f.Emitir("ana@ejemplo.test", time.Now())
	if err != nil {
		t.Fatalf("Emitir devolvió error: %v", err)
	}

	partes := strings.Split(token, ".")
	if len(partes) != 3 {
		t.Fatalf("el token tiene %d partes, se esperaban 3", len(partes))
	}

	crudo, err := base64.RawURLEncoding.DecodeString(partes[1])
	if err != nil {
		t.Fatalf("la carga no es base64url: %v", err)
	}

	var campos map[string]any
	if err := json.Unmarshal(crudo, &campos); err != nil {
		t.Fatalf("la carga no es JSON: %v", err)
	}

	for clave := range campos {
		if clave != "d" && clave != "x" {
			t.Errorf("el token lleva un campo inesperado (%q); recuerda que va firmado, no cifrado", clave)
		}
	}
}

// La firma es lo único que separa un token legítimo de uno inventado.
func TestUnTokenManipuladoNoVale(t *testing.T) {
	f := firmante(t)
	ahora := time.Now()

	token, _, err := f.Emitir("ana@ejemplo.test", ahora)
	if err != nil {
		t.Fatalf("Emitir devolvió error: %v", err)
	}
	partes := strings.Split(token, ".")

	// Una carga distinta —otro destino— con la firma original.
	otroDestino := base64.RawURLEncoding.EncodeToString(
		[]byte(`{"d":"intruso@ejemplo.test","x":9999999999}`))

	casos := map[string]string{
		"otro destino con la firma ajena": partes[0] + "." + otroDestino + "." + partes[2],
		"firma cambiada":                  partes[0] + "." + partes[1] + ".AAAA",
		"sin firma":                       partes[0] + "." + partes[1],
		"version desconocida":             "rv9." + partes[1] + "." + partes[2],
		"basura":                          "no-es-un-token",
		"vacio":                           "",
	}

	for nombre, token := range casos {
		t.Run(nombre, func(t *testing.T) {
			if _, err := f.Verificar(token, ahora); !errors.Is(err, identidad.ErrTokenInvalido) {
				t.Fatalf("se esperaba ErrTokenInvalido, se obtuvo %v", err)
			}
		})
	}
}

// Otra clave, otro firmante: un token de un despliegue no vale en otro.
func TestUnTokenDeOtraClaveNoVale(t *testing.T) {
	ahora := time.Now()

	ajeno := identidad.NuevoFirmante([]byte("otra-clave-distinta"), 15*time.Minute)
	token, _, err := ajeno.Emitir("ana@ejemplo.test", ahora)
	if err != nil {
		t.Fatalf("Emitir devolvió error: %v", err)
	}

	if _, err := firmante(t).Verificar(token, ahora); !errors.Is(err, identidad.ErrTokenInvalido) {
		t.Fatalf("se esperaba ErrTokenInvalido, se obtuvo %v", err)
	}
}

// Caducar SÍ se distingue de ser inválido, y es deliberado: la interfaz tiene
// que poder decir "vuelve a identificarte" en vez de "no eres tú".
func TestUnTokenCaducadoSeDistingueDeUnoInvalido(t *testing.T) {
	f := firmante(t)
	ahora := time.Now()

	token, expira, err := f.Emitir("ana@ejemplo.test", ahora)
	if err != nil {
		t.Fatalf("Emitir devolvió error: %v", err)
	}

	if _, err := f.Verificar(token, expira.Add(time.Second)); !errors.Is(err, identidad.ErrTokenVencido) {
		t.Fatalf("se esperaba ErrTokenVencido, se obtuvo %v", err)
	}

	// Justo antes de caducar todavía vale: el límite es >=, no >.
	if _, err := f.Verificar(token, expira.Add(-time.Second)); err != nil {
		t.Fatalf("el token debía valer un segundo antes de caducar: %v", err)
	}
}
