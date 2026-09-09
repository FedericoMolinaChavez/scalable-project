package identidad_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
)

// El hash de una contraseña, sin base de datos de por medio.
//
// Estas sí se pueden probar sin infraestructura, al revés que casi todo lo
// demás de este paquete: lo que comprueban vive entero en Go.

func TestElHashVerificaLaContrasenaYNingunaOtra(t *testing.T) {
	const contrasena = "una-frase-larga-de-prueba"

	hash, err := identidad.HashContrasena(contrasena)
	if err != nil {
		t.Fatalf("HashContrasena devolvió error: %v", err)
	}

	coincide, err := identidad.VerificarContrasena(hash, contrasena)
	if err != nil {
		t.Fatalf("VerificarContrasena devolvió error: %v", err)
	}
	if !coincide {
		t.Fatal("la contraseña correcta no verificó")
	}

	coincide, err = identidad.VerificarContrasena(hash, "una-frase-larga-de-pruebA")
	if err != nil {
		t.Fatalf("VerificarContrasena devolvió error: %v", err)
	}
	if coincide {
		t.Fatal("verificó una contraseña que difiere en un byte")
	}
}

// La sal es por fila. Sin ella, dos personas con la misma contraseña tendrían
// el mismo hash, y quien leyera la tabla sabría cuáles repiten sin romper
// ninguna: una tabla arcoíris se construye una vez y sirve para todas.
func TestDosHashesDeLaMismaContrasenaSonDistintos(t *testing.T) {
	const contrasena = "una-frase-larga-de-prueba"

	uno, err := identidad.HashContrasena(contrasena)
	if err != nil {
		t.Fatalf("HashContrasena devolvió error: %v", err)
	}
	otro, err := identidad.HashContrasena(contrasena)
	if err != nil {
		t.Fatalf("HashContrasena devolvió error: %v", err)
	}

	if uno == otro {
		t.Fatal("dos hashes de la misma contraseña salieron iguales: falta la sal")
	}
}

// Los parámetros viajan DENTRO del hash, en formato PHC. Es lo que permite
// subir el coste más adelante sin invalidar lo ya guardado: cada fila se
// verifica con los parámetros con los que se escribió.
func TestElHashLlevaSusParametrosDentro(t *testing.T) {
	hash, err := identidad.HashContrasena("una-frase-larga-de-prueba")
	if err != nil {
		t.Fatalf("HashContrasena devolvió error: %v", err)
	}

	if !strings.HasPrefix(hash, "$argon2id$v=19$m=") {
		t.Fatalf("el hash no tiene formato PHC de argon2id: %q", hash)
	}
	if partes := strings.Split(hash, "$"); len(partes) != 6 {
		t.Fatalf("el hash tiene %d segmentos y el formato PHC son 6: %q", len(partes), hash)
	}
}

// Un hash ilegible NO es "contraseña incorrecta". Devolverlo como tal
// escondería una fila corrupta: quien no puede entrar reintentaría para siempre
// sin que nada apareciera en los registros.
func TestUnHashIlegibleEsUnErrorYNoUnRechazo(t *testing.T) {
	casos := []string{
		"",
		"no-es-un-hash",
		"$argon2i$v=19$m=1,t=1,p=1$c2Fs$aGFzaA",  // otro algoritmo
		"$argon2id$v=99$m=1,t=1,p=1$c2Fs$aGFzaA", // otra versión
		"$argon2id$v=19$m=1,t=1,p=1$no-base64$aGFzaA",
	}

	for _, hash := range casos {
		coincide, err := identidad.VerificarContrasena(hash, "lo-que-sea")
		if !errors.Is(err, identidad.ErrHashIlegible) {
			t.Errorf("con %q se esperaba ErrHashIlegible y salió %v", hash, err)
		}
		if coincide {
			t.Errorf("con %q verificó como correcta", hash)
		}
	}
}
