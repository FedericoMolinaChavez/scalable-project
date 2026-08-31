package datos_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Estas pruebas corren contra la base real de deploy/docker-compose.yml, a
// través de PgBouncer en modo transacción. No se sustituye la base por un
// doble a propósito: lo que se comprueba aquí es precisamente lo que hace el
// motor —RLS, la restricción EXCLUDE, el SET LOCAL bajo un pooler que
// multiplexa conexiones— y ninguna de esas cosas existe en un doble.
//
// Sin DATABASE_URL se omiten, para que `go test ./...` siga siendo útil sin
// infraestructura levantada.

// La semilla crea dos tenants a propósito, para poder comprobar el aislamiento
// entre ellos. Los identificadores fijos salen de db/semillas/dev.sql.
const (
	tenantA = "11111111-1111-1111-1111-111111111111"
	tenantB = "99999999-9999-9999-9999-999999999999"

	// Ninguna fila de la semilla lleva este tenant.
	tenantInexistente = "00000000-0000-0000-0000-0000000000ff"

	servicioA        = "33333333-3333-3333-3333-333333333333"
	recursoA         = "44444444-4444-4444-4444-444444444444"
	politicaVersionA = "55555555-5555-5555-5555-555555555555"
)

func abrir(t *testing.T) *datos.BD {
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

// contarSedes cuenta lo visible bajo el contexto de tenant que se le pase.
func contarSedes(t *testing.T, bd *datos.BD, tenant string) int {
	t.Helper()

	var n int
	if err := bd.EnTenant(t.Context(), tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), "SELECT count(*) FROM negocio.sede").Scan(&n)
	}); err != nil {
		t.Fatalf("EnTenant(%s) devolvió error: %v", tenant, err)
	}
	return n
}

func nombresSedes(t *testing.T, bd *datos.BD, tenant string) []string {
	t.Helper()

	var nombres []string
	if err := bd.EnTenant(t.Context(), tenant, func(tx pgx.Tx) error {
		filas, err := tx.Query(t.Context(), "SELECT nombre FROM negocio.sede ORDER BY nombre")
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var n string
			if err := filas.Scan(&n); err != nil {
				return err
			}
			nombres = append(nombres, n)
		}
		return filas.Err()
	}); err != nil {
		t.Fatalf("EnTenant(%s) devolvió error: %v", tenant, err)
	}
	return nombres
}

// El contrato de db/README.md: con contexto de tenant se ven filas, y cada
// tenant ve las suyas y solo las suyas.
func TestEnTenantAislaEntreTenants(t *testing.T) {
	bd := abrir(t)

	deA := nombresSedes(t, bd, tenantA)
	deB := nombresSedes(t, bd, tenantB)

	if len(deA) == 0 {
		t.Fatal("el tenant A no vio ninguna sede; el contexto no se está aplicando")
	}
	if len(deB) == 0 {
		t.Fatal("el tenant B no vio ninguna sede; la semilla debería darle una")
	}

	// Lo que importa: los conjuntos son disjuntos. Si el SET LOCAL se filtrara
	// entre transacciones a través del pooler, aquí aparecerían mezclados.
	for _, a := range deA {
		for _, b := range deB {
			if a == b {
				t.Fatalf("la sede %q es visible para ambos tenants; el aislamiento no se sostiene", a)
			}
		}
	}

	// Y un tenant que no existe no ve nada, en vez de heredar lo anterior.
	if n := contarSedes(t, bd, tenantInexistente); n != 0 {
		t.Fatalf("un tenant inexistente vio %d sedes; el contexto se está filtrando", n)
	}
}

// Falla cerrado: sin tenant, SinTenant no ve negocio.* aunque la transacción
// sea válida. Es la diferencia entre no ver datos y ver los de otro.
func TestSinTenantNoVeNegocio(t *testing.T) {
	bd := abrir(t)
	ctx := t.Context()

	var n int
	if err := bd.SinTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT count(*) FROM negocio.sede").Scan(&n)
	}); err != nil {
		t.Fatalf("SinTenant devolvió error: %v", err)
	}

	if n != 0 {
		t.Fatalf("sin contexto de tenant se vieron %d sedes; RLS no está fallando cerrado", n)
	}
}

func TestEnTenantExigeTenant(t *testing.T) {
	bd := abrir(t)

	err := bd.EnTenant(t.Context(), "", func(pgx.Tx) error { return nil })
	if !errors.Is(err, datos.ErrSinTenant) {
		t.Fatalf("se esperaba ErrSinTenant, se obtuvo %v", err)
	}
}

// Un error dentro de fn revierte. Sin esto, un fallo a mitad de la ruta
// crítica dejaría la reserva insertada y el voucher sin consumir.
func TestEnTenantRevierteAlFallar(t *testing.T) {
	bd := abrir(t)
	ctx := t.Context()

	errEsperado := errors.New("fallo simulado")

	err := bd.EnTenant(ctx, tenantA, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO negocio.sede (tenant_id, id, nombre, zona_horaria)
			 VALUES ($1, gen_random_uuid(), 'sede que no debe quedar', 'America/Bogota')`,
			tenantA); err != nil {
			return err
		}
		return errEsperado
	})

	if !errors.Is(err, errEsperado) {
		t.Fatalf("se esperaba el error de fn, se obtuvo %v", err)
	}

	var quedan int
	if err := bd.EnTenant(ctx, tenantA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			"SELECT count(*) FROM negocio.sede WHERE nombre = 'sede que no debe quedar'").Scan(&quedan)
	}); err != nil {
		t.Fatalf("no se pudo verificar el rollback: %v", err)
	}

	if quedan != 0 {
		t.Fatalf("quedaron %d filas tras el rollback", quedan)
	}
}

// La traducción del 23P01: la invariante de RNF-10 llega al código como
// ErrHorarioOcupado y no como un error opaco del motor.
//
// Es el caso que más importa de este paquete. El núcleo no comprueba
// disponibilidad y luego inserta —eso sería una carrera—: inserta y traduce el
// rechazo del motor. Si esta traducción falla, RF-01 no puede distinguir
// "horario ocupado" de "error interno".
func TestSolapamientoDaErrHorarioOcupado(t *testing.T) {
	bd := abrir(t)
	ctx := t.Context()

	// Franja propia, lejos de la que usa db/pruebas/reserva_concurrente.sql,
	// para que ambas suites puedan correr sin pisarse.
	const (
		inicio = "2027-03-15 10:00:00-05"
		fin    = "2027-03-15 11:00:00-05"
	)

	insertar := func() error {
		return bd.EnTenant(ctx, tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO negocio.reserva (
					tenant_id, id, servicio_id, recurso_id,
					contacto_nombre, contacto_email,
					periodo, estado, expira_en,
					precio_cobrado, moneda, politica_version_id
				) VALUES (
					$1, gen_random_uuid(), $2, $3,
					'Prueba de solapamiento', 'solapamiento@ejemplo.test',
					tstzrange($4::timestamptz, $5::timestamptz, '[)'),
					'pendiente', now() + interval '10 minutes',
					80000.00, 'COP', $6
				)`,
				tenantA, servicioA, recursoA, inicio, fin, politicaVersionA)
			return err
		})
	}

	limpiar := func() {
		_ = bd.EnTenant(context.Background(), tenantA, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				`DELETE FROM negocio.reserva
				 WHERE recurso_id = $1
				   AND periodo && tstzrange($2::timestamptz, $3::timestamptz, '[)')`,
				recursoA, inicio, fin)
			return err
		})
	}
	limpiar()
	t.Cleanup(limpiar)

	if err := insertar(); err != nil {
		t.Fatalf("la primera inserción debía funcionar: %v (SQLSTATE %q)", err, datos.CodigoPG(err))
	}

	// La segunda pisa el mismo cupo: la restricción EXCLUDE la rechaza.
	err := insertar()
	if !errors.Is(err, datos.ErrHorarioOcupado) {
		t.Fatalf("se esperaba ErrHorarioOcupado, se obtuvo %v (SQLSTATE %q)", err, datos.CodigoPG(err))
	}

	// Y el código del motor sigue alcanzable bajo el error traducido: la
	// traducción envuelve con %w, no sustituye.
	if codigo := datos.CodigoPG(err); codigo != "23P01" {
		t.Fatalf("se esperaba SQLSTATE 23P01 bajo el error traducido, se obtuvo %q", codigo)
	}
}
