// Package pruebas da a las pruebas de integración la base real y los
// identificadores de la semilla.
//
// Importa `testing` a propósito, igual que net/http/httptest: es un paquete de
// apoyo para pruebas, no código de producción, y ningún binario de cmd/ lo
// enlaza.
//
// Existe porque las pruebas que importan de este backend no se pueden escribir
// contra un doble. Lo que comprueban —RLS, la restricción EXCLUDE, el SET LOCAL
// bajo un pooler que multiplexa conexiones, el cálculo de disponibilidad en
// SQL— no existe fuera de PostgreSQL. Un doble las haría pasar sin comprobar
// nada.
package pruebas

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Identificadores fijos de db/semillas/dev.sql. Son fijos precisamente para que
// las pruebas puedan referenciarlos sin descubrirlos primero.
//
// Las pruebas escriben en SU PROPIO tenant, no en el de demostración, y no es
// una manía de aislamiento: transicion_estado es append-only por trigger
// (RF-28), así que una reserva creada por el núcleo no se puede borrar después
// sin desactivar la garantía que la prueba quiere que siga en pie. Lo más que
// puede hacer la limpieza es cancelarla, y eso significa que cada pasada deja
// filas para siempre. En el tenant de demostración esas filas acababan siendo
// lo que enseñaban las pantallas.
const (
	// Tenant de las pruebas. Mismo horario, moneda y servicio que el de
	// demostración, para que las afirmaciones sobre franjas y precios sigan
	// valiendo.
	Tenant   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	Sede     = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	Servicio = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	Recurso  = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	Politica = "eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee"

	// DuracionServicio en minutos, según la semilla.
	DuracionServicio = 60

	// TenantAjeno es cualquier otro tenant, para comprobar que no se ve nada
	// suyo. Es el de demostración: sirve como "el de al lado" sin que las
	// pruebas le escriban nada.
	TenantAjeno = "11111111-1111-1111-1111-111111111111"
)

// AbrirBD conecta con la base de deploy/docker-compose.yml a través de
// PgBouncer, u omite la prueba si no hay ninguna.
//
// Se omite en vez de fallar para que `go test ./...` siga siendo útil sin
// infraestructura levantada. En CI la infraestructura está, así que allí no se
// omite nada.
func AbrirBD(t *testing.T) *datos.BD {
	t.Helper()

	url := os.Getenv("DATABASE_URL")
	if url == "" {
		t.Skip("sin DATABASE_URL: se omite (levanta con `task infra:up`)")
	}

	ctx, cancelar := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelar()

	bd, err := datos.Abrir(ctx, url)
	if err != nil {
		t.Fatalf("no se pudo abrir la base: %v", err)
	}
	t.Cleanup(bd.Cerrar)

	return bd
}

// LimpiarFranja deja libre el cupo de [desde, hasta) sobre un recurso.
//
// Se llama al empezar Y al terminar. Al terminar es evidente; al empezar hace
// falta porque una ejecución interrumpida —un Ctrl-C, un fallo a mitad— deja
// filas que harían fallar la siguiente con un 409 que no tiene nada que ver con
// lo que se está probando.
//
// No borra: cancela. Borrar la reserva exigiría borrar antes su historia en
// transicion_estado, y esa tabla es append-only por trigger (RF-28), así que ni
// el rol de la aplicación ni ningún otro puede tocarla sin desactivar la
// garantía que la prueba quiere que siga en pie. Cancelar saca la fila del
// predicado de la restricción EXCLUDE —solo 'pendiente' y 'confirmada' ocupan
// cupo—, que es lo único que la prueba siguiente necesita, y deja la historia
// intacta.
func LimpiarFranja(t *testing.T, bd *datos.BD, tenant, recurso string, desde, hasta time.Time) {
	t.Helper()

	limpiar := func() {
		ctx := context.Background()

		err := bd.EnTenant(ctx, tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE negocio.reserva
				SET estado = 'cancelada'
				WHERE recurso_id = $1
				  AND estado IN ('pendiente', 'confirmada')
				  AND periodo && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
				recurso, desde, hasta)
			return err
		})
		if err != nil {
			t.Fatalf("no se pudo limpiar la franja de prueba: %v", err)
		}
	}

	limpiar()
	t.Cleanup(limpiar)
}

// Un día de la semana sembrada por paquete de pruebas.
//
// El reparto no es decorativo: `go test` corre los paquetes en PARALELO y la
// semilla tiene un solo recurso, así que dos paquetes reservando el mismo día
// se estorbarían con 409 y con recuentos cambiantes que no tienen nada que ver
// con lo que cada uno comprueba. Un día por paquete los mantiene independientes
// sin serializar nada.
//
//	nucleo         → Lunes
//	consulta       → Martes
//	disponibilidad → Miercoles (y Domingo, para el día cerrado)
//
// La semilla abre de lunes a viernes, de 09:00 a 17:00 locales.
//
// La conversión va con un desplazamiento fijo de -05:00 y no con
// time.LoadLocation("America/Bogota"): Colombia no observa horario de verano,
// así que el desplazamiento es constante, y LoadLocation necesita la base de
// datos de zonas horarias del sistema, que Windows no trae. Una prueba que solo
// pasa en Linux no sirve para desarrollar aquí.
func Lunes(hora int) time.Time {
	bogota := time.FixedZone("-05", -5*60*60)

	// 2028-01-10 es lunes. Está lejos de cualquier dato de desarrollo, así que
	// una prueba no puede chocar con una reserva hecha a mano al probar algo, y
	// está en el futuro, que es lo único que la disponibilidad ofrece.
	return time.Date(2028, time.January, 10, hora, 0, 0, 0, bogota)
}

func Martes(hora int) time.Time    { return Lunes(hora).Add(24 * time.Hour) }
func Miercoles(hora int) time.Time { return Lunes(hora).Add(48 * time.Hour) }

// Domingo es el día anterior: la semilla no abre, así que sirve para comprobar
// que "cerrado" es una lista vacía y no un error.
func Domingo(hora int) time.Time { return Lunes(hora).Add(-24 * time.Hour) }
