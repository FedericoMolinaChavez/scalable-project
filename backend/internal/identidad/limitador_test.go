package identidad_test

import (
	"os"
	"testing"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
)

// limitador da a las pruebas el Valkey de verdad.
//
// No se sustituye por un doble. Lo que el límite tiene de delicado está EN
// Valkey: que el contador y su caducidad se impongan en una sola ida, que la
// ventana caduque sola, y que dos peticiones simultáneas no lean el mismo
// número. Un doble en memoria haría pasar las pruebas sin comprobar nada de
// eso, igual que un doble de PostgreSQL no comprobaría la restricción EXCLUDE.
func limitador(t *testing.T) *cache.Cliente {
	t.Helper()

	url := os.Getenv("VALKEY_URL")
	if url == "" {
		url = "redis://localhost:6379"
	}

	cliente, err := cache.Abrir(url)
	if err != nil {
		t.Skipf("sin Valkey: se omite (levanta con `task infra:up`): %v", err)
	}
	t.Cleanup(cliente.Cerrar)

	if err := cliente.Comprobar(t.Context()); err != nil {
		t.Skipf("Valkey no responde: se omite (levanta con `task infra:up`): %v", err)
	}

	return cliente
}
