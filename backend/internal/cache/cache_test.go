package cache_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
)

// Contra el Valkey de verdad, no contra un doble. Lo que este paquete tiene de
// delicado vive EN Valkey: que el contador y su caducidad se impongan en una
// sola ida, que la ventana expire sola, y que la vigencia de una clave sea
// obligatoria. Un mapa en memoria haría pasar todo esto sin comprobar nada,
// igual que un doble de PostgreSQL no comprobaría la restricción EXCLUDE.

func cliente(t *testing.T) *cache.Cliente {
	t.Helper()

	url := os.Getenv("VALKEY_URL")
	if url == "" {
		url = "redis://localhost:6379"
	}

	c, err := cache.Abrir(url)
	if err != nil {
		t.Skipf("sin Valkey: se omite (levanta con `task infra:up`): %v", err)
	}
	t.Cleanup(c.Cerrar)

	if err := c.Comprobar(t.Context()); err != nil {
		t.Skipf("Valkey no responde: se omite (levanta con `task infra:up`): %v", err)
	}

	return c
}

// clave nueva por prueba Y por ejecución: las claves caducan solas, pero una
// prueba no debe depender de que la anterior ya haya caducado.
func clave(t *testing.T) string {
	t.Helper()
	return "prb:" + t.Name() + ":" + uuid.NewString()[:8]
}

func TestGuardarYLeer(t *testing.T) {
	c := cliente(t)
	k := clave(t)

	if _, err := c.Leer(t.Context(), k); !errors.Is(err, cache.ErrVacio) {
		t.Fatalf("una clave que no existe debía dar ErrVacio, dio %v", err)
	}

	if err := c.Guardar(t.Context(), k, []byte("hola"), time.Minute); err != nil {
		t.Fatalf("Guardar devolvió error: %v", err)
	}

	valor, err := c.Leer(t.Context(), k)
	if err != nil {
		t.Fatalf("Leer devolvió error: %v", err)
	}
	if string(valor) != "hola" {
		t.Fatalf("volvió %q en vez de \"hola\"", valor)
	}
}

// Una clave sin caducidad es una fuga de memoria con buena letra: nadie se
// acuerda de borrarla y crece hasta que Valkey empieza a expulsar lo que sí
// hacía falta.
func TestNoSeAdmitenClavesEternas(t *testing.T) {
	c := cliente(t)

	for _, vigencia := range []time.Duration{0, -time.Second} {
		if err := c.Guardar(t.Context(), clave(t), []byte("x"), vigencia); err == nil {
			t.Fatalf("se aceptó una vigencia de %s", vigencia)
		}
	}
}

// La vigencia se respeta de verdad, no solo se pasa como argumento.
func TestElValorCaduca(t *testing.T) {
	c := cliente(t)
	k := clave(t)

	if err := c.Guardar(t.Context(), k, []byte("efímero"), 100*time.Millisecond); err != nil {
		t.Fatalf("Guardar devolvió error: %v", err)
	}

	time.Sleep(250 * time.Millisecond)

	if _, err := c.Leer(t.Context(), k); !errors.Is(err, cache.ErrVacio) {
		t.Fatalf("el valor seguía vivo tras caducar: %v", err)
	}
}

// La cuota deja pasar exactamente `maximo` y corta a partir de ahí.
func TestLaCuotaCortaEnElMaximo(t *testing.T) {
	c := cliente(t)
	k := clave(t)

	for i := range 3 {
		if v := c.Permite(t.Context(), k, 3, time.Minute, cache.Denegar); !v.Permitido {
			t.Fatalf("la petición %d se rechazó y el máximo era 3", i+1)
		}
	}

	v := c.Permite(t.Context(), k, 3, time.Minute, cache.Denegar)
	if v.Permitido {
		t.Fatal("la cuarta petición pasó con un máximo de 3")
	}

	// Retry-After sale de aquí. Sin un valor útil, un cliente educado no sabe
	// cuándo reintentar y acaba haciéndolo en bucle, que es lo contrario de lo
	// que el límite busca.
	if v.Espera <= 0 || v.Espera > time.Minute {
		t.Fatalf("la espera devuelta es %s; se esperaba algo dentro de la ventana", v.Espera)
	}
}

// La ventana caduca sola: sin esto, un destino que se pasa una vez queda
// bloqueado para siempre.
func TestLaVentanaSeAbreSola(t *testing.T) {
	c := cliente(t)
	k := clave(t)

	if v := c.Permite(t.Context(), k, 1, 150*time.Millisecond, cache.Denegar); !v.Permitido {
		t.Fatal("la primera petición se rechazó")
	}
	if v := c.Permite(t.Context(), k, 1, 150*time.Millisecond, cache.Denegar); v.Permitido {
		t.Fatal("la segunda pasó dentro de la misma ventana")
	}

	time.Sleep(300 * time.Millisecond)

	if v := c.Permite(t.Context(), k, 1, 150*time.Millisecond, cache.Denegar); !v.Permitido {
		t.Fatal("la ventana no se abrió al caducar; el destino quedaría bloqueado para siempre")
	}
}

// Con Valkey inalcanzable, cada ruta elige su desenlace. No hay una respuesta
// buena para las dos: fallar abierto pierde la protección, fallar cerrado tumba
// la ruta, y cuál duele menos depende de si hay otra capa detrás.
func TestElDesenlaceCuandoValkeyNoResponde(t *testing.T) {
	// Puerto donde no escucha nadie: el cliente se crea pero ningún comando
	// llega.
	caido, err := cache.Abrir("redis://localhost:6399")
	if err != nil {
		t.Skipf("el cliente ni siquiera se pudo construir: %v", err)
	}
	t.Cleanup(caido.Cerrar)

	ctx, cancelar := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancelar()

	if v := caido.Permite(ctx, "x", 1, time.Minute, cache.Permitir); !v.Permitido {
		t.Error("con AlFallar=Permitir debía dejar pasar: detrás hay otras capas de RNF-08")
	}
	if v := caido.Permite(ctx, "x", 1, time.Minute, cache.Denegar); v.Permitido {
		t.Error("con AlFallar=Denegar debía cortar: detrás del envío de códigos no hay ninguna capa")
	}
}
