package consulta_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// El martes es de este paquete. Ver pruebas.Martes: los paquetes corren en
// paralelo sobre el único recurso de la semilla.
const reservasSembradas = 5

// sembrar deja exactamente reservasSembradas pendientes en horas consecutivas
// del martes, y devuelve la ventana que las contiene a todas.
//
// Las pruebas de este archivo filtran SIEMPRE por esa ventana y por el estado
// pendiente. Sin acotar, contarían también las reservas que cualquiera haya
// creado a mano en la base de desarrollo, y el resultado dependería de con qué
// se hubiera estado jugando esa tarde.
func sembrar(t *testing.T, bd *datos.BD) (desde, hasta time.Time) {
	t.Helper()

	desde, hasta = pruebas.Martes(9), pruebas.Martes(9+reservasSembradas)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, desde, hasta)

	for i := range reservasSembradas {
		inicio := pruebas.Martes(9 + i)

		if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `
				INSERT INTO negocio.reserva (
					tenant_id, servicio_id, recurso_id,
					contacto_nombre, contacto_email,
					periodo, estado, expira_en,
					precio_cobrado, moneda, politica_version_id
				) VALUES (
					$1, $2, $3, 'Paginación', 'paginacion@ejemplo.test',
					tstzrange($4::timestamptz, $5::timestamptz, '[)'),
					'pendiente', now() + interval '1 hour',
					80000.00, 'COP', $6
				)`,
				pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
				inicio, inicio.Add(time.Hour), pruebas.Politica)
			return err
		}); err != nil {
			t.Fatalf("no se pudo sembrar la reserva %d: %v", i, err)
		}
	}

	return desde, hasta
}

func filtroBase(desde, hasta time.Time) consulta.Filtro {
	return consulta.Filtro{
		Estados: []api.EstadoReserva{api.Pendiente},
		Desde:   &desde,
		Hasta:   &hasta,
	}
}

// Recorrer todas las páginas devuelve cada reserva una vez y ninguna dos veces.
//
// Es lo que compra la paginación por cursor frente a OFFSET: con desplazamiento,
// insertar una fila mientras se pagina corre el resto y el cliente ve
// duplicados o se salta filas. Aquí la posición es la última fila entregada, no
// un número de orden.
func TestPaginacionRecorreTodoSinRepetir(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	desde, hasta := sembrar(t, bd)

	filtro := filtroBase(desde, hasta)
	filtro.Limite = 2

	vistas := map[uuid.UUID]bool{}
	paginas := 0

	for {
		pagina, err := svc.Listar(t.Context(), tenant, filtro)
		if err != nil {
			t.Fatalf("Listar devolvió error: %v", err)
		}

		for _, reserva := range pagina.Datos {
			if vistas[reserva.Id] {
				t.Fatalf("la reserva %s salió en dos páginas", reserva.Id)
			}
			vistas[reserva.Id] = true
		}

		paginas++
		if paginas > reservasSembradas+2 {
			t.Fatal("la paginación no termina; el cursor no está avanzando")
		}

		if pagina.SiguienteCursor == nil {
			break
		}
		if len(pagina.Datos) != filtro.Limite {
			t.Fatalf("una página intermedia trajo %d filas con límite %d", len(pagina.Datos), filtro.Limite)
		}
		filtro.Cursor = *pagina.SiguienteCursor
	}

	if len(vistas) != reservasSembradas {
		t.Fatalf("se recorrieron %d reservas; se sembraron %d", len(vistas), reservasSembradas)
	}
}

// La última página no emite cursor. Emitirlo siempre que la página venga llena
// produce una página vacía extra cuando el total es múltiplo del límite, y el
// cliente no puede distinguirla de un fallo.
func TestUltimaPaginaNoEmiteCursor(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	desde, hasta := sembrar(t, bd)

	// Límite exactamente igual al total: la página viene llena y aun así no
	// queda nada detrás.
	filtro := filtroBase(desde, hasta)
	filtro.Limite = reservasSembradas

	pagina, err := svc.Listar(t.Context(), tenant, filtro)
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}

	if len(pagina.Datos) != reservasSembradas {
		t.Fatalf("la página trajo %d filas; se esperaban %d", len(pagina.Datos), reservasSembradas)
	}
	if pagina.SiguienteCursor != nil {
		t.Fatal("se emitió cursor sin página siguiente; el cliente pediría una página vacía")
	}
}

// El orden es de la más reciente a la más antigua, y lo sostiene el par
// (creada_en, id): creada_en sola no es única, y con ~3.300 inserciones por
// segundo (RNF-03) dos reservas en el mismo microsegundo no son hipotéticas.
func TestOrdenDeMasRecienteAMasAntigua(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	desde, hasta := sembrar(t, bd)

	pagina, err := svc.Listar(t.Context(), tenant, filtroBase(desde, hasta))
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}

	for i := 1; i < len(pagina.Datos); i++ {
		anterior, actual := pagina.Datos[i-1], pagina.Datos[i]
		if actual.CreadaEn.After(anterior.CreadaEn) {
			t.Fatalf("la reserva %d es más reciente que la %d; el orden está invertido", i, i-1)
		}
	}
}

func TestCursorIlegibleSeRechaza(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	for _, caso := range []struct {
		nombre string
		cursor string
	}{
		{"no es base64", "%%%%"},
		{"base64 sin separador", "c2luLXNlcGFyYWRvcg"},
		{"marca de tiempo ilegible", "YXllcnwxMTExMTExMS0xMTExLTExMTEtMTExMS0xMTExMTExMTExMTE"},
	} {
		t.Run(caso.nombre, func(t *testing.T) {
			_, err := svc.Listar(t.Context(), tenant, consulta.Filtro{Cursor: caso.cursor})
			if !errors.Is(err, consulta.ErrCursorInvalido) {
				t.Fatalf("se esperaba ErrCursorInvalido, se obtuvo %v", err)
			}
		})
	}
}

// El límite se acota en el servicio y no solo en el contrato. El contrato lo
// declara para el cliente generado; un cliente que no lo use —o una llamada
// interna— pediría cien mil filas y el contrato no lo impediría.
func TestLimiteSeAcota(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)

	desde, hasta := sembrar(t, bd)

	filtro := filtroBase(desde, hasta)
	filtro.Limite = 10_000

	pagina, err := svc.Listar(t.Context(), tenant, filtro)
	if err != nil {
		t.Fatalf("Listar devolvió error: %v", err)
	}
	if len(pagina.Datos) > consulta.LimiteMaximo {
		t.Fatalf("se devolvieron %d filas; el máximo es %d", len(pagina.Datos), consulta.LimiteMaximo)
	}
}

// Obtener una reserva que no existe no es una reserva vacía.
func TestObtenerInexistenteDaNoEncontrado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := consulta.Nuevo(bd)

	_, err := svc.Obtener(t.Context(),
		uuid.MustParse(pruebas.Tenant),
		uuid.MustParse("00000000-0000-0000-0000-0000000000cc"))

	if !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se esperaba ErrNoEncontrado, se obtuvo %v", err)
	}
}
