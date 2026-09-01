package disponibilidad_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// El caché es lo que hace sostenible el 90% del tráfico de RNF-03: un acierto
// no toca PostgreSQL en absoluto.
//
// Aquí SÍ se usa un doble, al revés que en el resto del paquete, y por una
// razón concreta: lo que se comprueba no es qué guarda Valkey —eso lo prueba
// Valkey— sino **cuántas veces se baja al motor**, y eso solo se puede contar
// interponiéndose. El caché de verdad tiene su prueba de humo abajo.

// cacheEspia cuenta accesos y permite simular un Valkey caído.
type cacheEspia struct {
	mu       sync.Mutex
	valores  map[string][]byte
	lecturas int
	escritas int

	// roto simula Valkey caído: todo falla, y la disponibilidad tiene que
	// seguir respondiendo igualmente.
	roto bool
}

func nuevoEspia() *cacheEspia {
	return &cacheEspia{valores: map[string][]byte{}}
}

func (c *cacheEspia) Leer(_ context.Context, clave string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.lecturas++
	if c.roto {
		return nil, errors.New("valkey caído")
	}
	if valor, hay := c.valores[clave]; hay {
		return valor, nil
	}
	return nil, cache.ErrVacio
}

func (c *cacheEspia) Guardar(_ context.Context, clave string, valor []byte, vigencia time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.escritas++
	if c.roto {
		return errors.New("valkey caído")
	}
	if vigencia <= 0 {
		return errors.New("vigencia inválida")
	}
	c.valores[clave] = valor
	return nil
}

func (c *cacheEspia) contadores() (lecturas, escritas int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lecturas, c.escritas
}

func conEspia(t *testing.T) (*disponibilidad.Servicio, *cacheEspia, uuid.UUID, uuid.UUID) {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	espia := nuevoEspia()

	return disponibilidad.Nuevo(bd, espia, registroMudo()), espia,
		uuid.MustParse(pruebas.Tenant), uuid.MustParse(pruebas.Servicio)
}

// La segunda consulta idéntica no baja al motor: se sirve del caché.
func TestLaSegundaConsultaSaleDelCache(t *testing.T) {
	svc, espia, tenant, srv := conEspia(t)
	dia := pruebas.Miercoles(0)

	primera, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}
	if _, escritas := espia.contadores(); escritas != 1 {
		t.Fatalf("la primera consulta guardó %d veces; se esperaba 1", escritas)
	}

	segunda, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	// Si hubiera vuelto al motor, habría guardado otra vez.
	if _, escritas := espia.contadores(); escritas != 1 {
		t.Fatalf("la segunda consulta volvió a bajar al motor: %d escrituras", escritas)
	}

	if len(primera.Franjas) != len(segunda.Franjas) {
		t.Fatalf("el caché devolvió %d franjas y el motor %d",
			len(segunda.Franjas), len(primera.Franjas))
	}
}

// `calculada_en` NO se resella al servir desde el caché.
//
// Es la diferencia entre un campo útil y uno decorativo: el cliente lo usa para
// saber cuánta desactualización está viendo, y si cada respuesta dijera "recién
// calculado" no podría saber que mira algo de hace dos segundos.
func TestCalculadaEnNoSeReselllaAlServirDelCache(t *testing.T) {
	svc, _, tenant, srv := conEspia(t)
	dia := pruebas.Miercoles(0)

	primera, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	segunda, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	if !segunda.CalculadaEn.Equal(primera.CalculadaEn) {
		t.Fatalf("calculada_en cambió al servir del caché (%s -> %s); el cliente no puede saber la desactualización que ve",
			primera.CalculadaEn, segunda.CalculadaEn)
	}
}

// La vigencia es la que RNF-10 autoriza y ni un milisegundo más.
func TestLaVigenciaEsLaDeRNF10(t *testing.T) {
	if disponibilidad.Staleness != 2*time.Second {
		t.Fatalf("la vigencia del caché es %s; RNF-10 autoriza 2 s", disponibilidad.Staleness)
	}
}

// La clave lleva el tenant, así que dos negocios no comparten proyección.
//
// El caché está POR ENCIMA de RLS: la política del motor no puede protegerlo,
// porque un acierto no llega a ejecutar ninguna consulta. Si la clave se
// quedara sin tenant, se serviría lo de un negocio a otro sin que PostgreSQL
// llegara a enterarse.
func TestElCacheNoMezclaTenants(t *testing.T) {
	svc, espia, tenant, srv := conEspia(t)
	dia := pruebas.Miercoles(0)

	if _, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour)); err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	ajeno := uuid.MustParse(pruebas.TenantAjeno)
	if _, err := svc.Consultar(t.Context(), ajeno, srv, dia, dia.Add(24*time.Hour)); err == nil {
		t.Fatal("el otro tenant vio el servicio ajeno; debería no existir para él")
	}

	// Dos claves distintas: la del segundo tenant no acertó con la del primero.
	if _, escritas := espia.contadores(); escritas != 1 {
		t.Fatalf("escrituras inesperadas (%d): revisa que la clave incluya el tenant", escritas)
	}
}

// Con Valkey caído la disponibilidad SIGUE respondiendo.
//
// Es la propiedad que justifica que el caché no entre en /listo: acelera, no
// decide, y no puede impedir que se responda.
func TestConElCacheCaidoSeSirveIgual(t *testing.T) {
	svc, espia, tenant, srv := conEspia(t)
	espia.roto = true

	dia := pruebas.Miercoles(0)

	disp, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("con el caché caído la disponibilidad dejó de responder: %v", err)
	}
	if len(disp.Franjas) != 8 {
		t.Fatalf("salieron %d franjas con el caché caído; el motor debía dar 8", len(disp.Franjas))
	}

	// Y lo intentó: no es que se saltara el caché, es que sobrevivió a su fallo.
	if lecturas, _ := espia.contadores(); lecturas == 0 {
		t.Fatal("no se consultó el caché siquiera")
	}
}

// valkeyDePruebas da el cliente real, u omite la prueba si no hay Valkey.
func valkeyDePruebas(t *testing.T) *cache.Cliente {
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
		t.Skipf("Valkey no responde: se omite: %v", err)
	}

	return c
}

// Prueba de humo contra el Valkey de verdad: que la ida y vuelta funcione con
// el cliente real, no solo con el doble.
func TestElCacheDeVerdadGuardaYDevuelve(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	valkey := valkeyDePruebas(t)

	svc := disponibilidad.Nuevo(bd, valkey, registroMudo())
	tenant := uuid.MustParse(pruebas.Tenant)
	srv := uuid.MustParse(pruebas.Servicio)

	// Ventana propia para no compartir clave con otras pruebas.
	dia := pruebas.Miercoles(0).Add(7 * 24 * time.Hour)

	primera, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	segunda, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	if !segunda.CalculadaEn.Equal(primera.CalculadaEn) {
		t.Fatal("la segunda consulta no salió de Valkey: se recalculó")
	}
}
