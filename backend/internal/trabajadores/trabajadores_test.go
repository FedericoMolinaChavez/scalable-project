package trabajadores_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/trabajadores"
)

// El día de este paquete es el jueves. Ver pruebas.Jueves: `go test` corre los
// paquetes en paralelo sobre el único recurso del tenant de pruebas.

func mudo() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// sembrarPendiente deja una reserva pendiente cuyo bloqueo ya venció, tal como
// la dejaría un checkout abandonado.
func sembrarPendiente(t *testing.T, bd *datos.BD, inicio time.Time, vencida bool) string {
	t.Helper()

	expira := "now() + interval '10 minutes'"
	if vencida {
		expira = "now() - interval '1 minute'"
	}

	var id string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Trabajadores', 'trabajadores@ejemplo.test',
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'pendiente', `+expira+`,
				80000.00, 'COP', $6
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, inicio.Add(time.Hour), pruebas.Politica).Scan(&id)
	}); err != nil {
		t.Fatalf("no se pudo sembrar la reserva: %v", err)
	}
	return id
}

// esperarEstado corre pasadas hasta que la reserva llega al estado esperado.
//
// Varias pasadas y no una, porque FOR UPDATE SKIP LOCKED hace justo lo que
// promete: si otro expirador tiene la fila bloqueada en ese instante, esta
// pasada la salta. Eso pasa de verdad —basta con tener `task back:run --
// trabajadores` levantado mientras se corren las pruebas— y en producción es lo
// normal, porque hay varias réplicas.
//
// Lo que se comprueba no cambia: que tras pasar el expirador la reserva acaba
// en el estado que toca. Exigir que la consiga la PRIMERA pasada sería exigir
// que no haya nadie más trabajando, que es lo contrario de lo que este diseño
// busca.
func esperarEstado(t *testing.T, bd *datos.BD, bucle trabajadores.Bucle, id, esperado string) {
	t.Helper()

	for range 10 {
		if _, err := bucle.Pasada(t.Context()); err != nil {
			t.Fatalf("la pasada falló: %v", err)
		}
		if estadoDe(t, bd, id) == esperado {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	t.Fatalf("la reserva quedó en %q; se esperaba %q", estadoDe(t, bd, id), esperado)
}

func estadoDe(t *testing.T, bd *datos.BD, id string) string {
	t.Helper()

	var estado string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT estado::text FROM negocio.reserva WHERE id = $1", id).Scan(&estado)
	}); err != nil {
		t.Fatalf("no se pudo leer el estado: %v", err)
	}
	return estado
}

func transicionesDe(t *testing.T, bd *datos.BD, id, hacia string) int {
	t.Helper()

	var n int
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT count(*) FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_nuevo = $2::negocio.estado_reserva`,
			id, hacia).Scan(&n)
	}); err != nil {
		t.Fatalf("no se pudieron contar las transiciones: %v", err)
	}
	return n
}

// El expirador hace lo único que el motor no puede hacer solo: liberar el cupo
// de un bloqueo vencido.
//
// El predicado de la restricción EXCLUDE no puede excluir las pendientes
// vencidas —PostgreSQL exige un predicado inmutable y now() no lo es— así que
// sin esto el cupo sigue ocupado hasta que alguien pida justo esa franja.
func TestElExpiradorLiberaElCupo(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(9)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	id := sembrarPendiente(t, bd, inicio, true)

	bucle := trabajadores.Expirador(bd, time.Second, mudo())
	esperarEstado(t, bd, bucle, id, "expirada")

	// Y con su historia (RF-28): un cambio de estado sin transición deja una
	// reserva expirada sin que nada explique quién la expiró.
	if n := transicionesDe(t, bd, id, "expirada"); n != 1 {
		t.Errorf("se escribieron %d transiciones a 'expirada'; se esperaba 1", n)
	}
}

// Un bloqueo VIGENTE no se toca. Es la otra mitad: un expirador que se pasa de
// listo cancelaría reservas que alguien está pagando en ese momento.
func TestElExpiradorNoTocaLoQueSigueVigente(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(10)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	id := sembrarPendiente(t, bd, inicio, false)

	bucle := trabajadores.Expirador(bd, time.Second, mudo())
	if _, err := bucle.Pasada(t.Context()); err != nil {
		t.Fatalf("la pasada falló: %v", err)
	}

	if estado := estadoDe(t, bd, id); estado != "pendiente" {
		t.Fatalf("un bloqueo vigente pasó a %q", estado)
	}
}

// Dos réplicas a la vez no duplican el trabajo.
//
// Es la prueba que justifica el FOR UPDATE SKIP LOCKED. Sin él, las dos
// seleccionan las mismas filas: la segunda espera a la primera y, cuando entra,
// vuelve a aplicar la transición sobre lo que ya estaba hecho. El síntoma sería
// una segunda fila en transicion_estado, que es append-only y no se puede
// deshacer.
func TestDosExpiradoresNoDuplicanTransiciones(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(11)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	id := sembrarPendiente(t, bd, inicio, true)

	uno := trabajadores.Expirador(bd, time.Second, mudo())
	otro := trabajadores.Expirador(bd, time.Second, mudo())

	var espera sync.WaitGroup
	espera.Add(2)
	for _, bucle := range []trabajadores.Bucle{uno, otro} {
		go func(b trabajadores.Bucle) {
			defer espera.Done()
			_, _ = b.Pasada(context.Background())
		}(bucle)
	}
	espera.Wait()

	if estado := estadoDe(t, bd, id); estado != "expirada" {
		t.Fatalf("el bloqueo quedó en %q", estado)
	}
	if n := transicionesDe(t, bd, id, "expirada"); n != 1 {
		t.Fatalf("dos réplicas escribieron %d transiciones; el SKIP LOCKED debía dejar una", n)
	}
}

// El expirador emite su evento, y en la misma transacción: el cupo vuelve a
// estar libre y hay quien quiere enterarse (RF-37, cuando exista).
func TestElExpiradorEmiteSuEvento(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := pruebas.Jueves(12)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	id := sembrarPendiente(t, bd, inicio, true)

	bucle := trabajadores.Expirador(bd, time.Second, mudo())
	esperarEstado(t, bd, bucle, id, "expirada")

	var eventos int
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT count(*) FROM negocio.outbox_evento
			WHERE tipo = 'reserva.expirada'
			  AND payload->>'reserva_id' = $1`, id).Scan(&eventos)
	}); err != nil {
		t.Fatalf("no se pudieron contar los eventos: %v", err)
	}
	if eventos != 1 {
		t.Fatalf("se emitieron %d eventos de expiración; se esperaba 1", eventos)
	}
}

// Una cita que nadie registró acaba en `no_show`, no en `completada`.
//
// Es la transición que RF-28 asigna al sistema. La otra ruta —pasar por
// `en_curso`— exige que un administrador registre la llegada (RF-32), y el
// sistema no puede inventarse que alguien apareció solo porque dieron las diez.
func TestUnaCitaQueNadieRegistroAcabaEnNoShow(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	// Una cita que empezó hace dos horas y terminó hace una: le tocan las dos
	// transiciones, en_curso primero y completada después.
	inicio := time.Now().Add(-2 * time.Hour)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	var id string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Pasada', 'pasada@ejemplo.test',
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'confirmada', NULL,
				80000.00, 'COP', $6
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, inicio.Add(time.Hour), pruebas.Politica).Scan(&id)
	}); err != nil {
		t.Fatalf("no se pudo sembrar la cita pasada: %v", err)
	}

	t.Cleanup(func() {
		_ = bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				"UPDATE negocio.reserva SET estado='cancelada' WHERE id=$1", id)
			return err
		})
	})

	// Umbral de quince minutos: la cita empezó hace dos horas, así que lo supera
	// con creces.
	bucle := trabajadores.Transiciones(bd, time.Second, 15*time.Minute, mudo())
	esperarEstado(t, bd, bucle, id, "no_show")
	if n := transicionesDe(t, bd, id, "no_show"); n != 1 {
		t.Errorf("se escribieron %d transiciones a no_show; se esperaba 1", n)
	}

	// Y NUNCA pasó por en_curso: a ese estado solo se llega por el check-in de
	// un administrador (RF-32).
	if n := transicionesDe(t, bd, id, "en_curso"); n != 0 {
		t.Errorf("el sistema marcó la llegada por su cuenta (%d transiciones a en_curso)", n)
	}
}

// Dentro del umbral no se toca: alguien puede estar llegando tarde, y marcarlo
// ausente tiene consecuencias sobre el reembolso de RF-29.
func TestDentroDelUmbralLaCitaSigueConfirmada(t *testing.T) {
	bd := pruebas.AbrirBD(t)

	inicio := time.Now().Add(-5 * time.Minute)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, inicio.Add(time.Hour))

	var id string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Tarde', 'tarde@ejemplo.test',
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'confirmada', NULL,
				80000.00, 'COP', $6
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, inicio.Add(time.Hour), pruebas.Politica).Scan(&id)
	}); err != nil {
		t.Fatalf("no se pudo sembrar: %v", err)
	}

	t.Cleanup(func() {
		_ = bd.EnTenant(context.Background(), pruebas.Tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(),
				"UPDATE negocio.reserva SET estado='cancelada' WHERE id=$1", id)
			return err
		})
	})

	bucle := trabajadores.Transiciones(bd, time.Second, 15*time.Minute, mudo())
	if _, err := bucle.Pasada(t.Context()); err != nil {
		t.Fatalf("la pasada falló: %v", err)
	}

	if estado := estadoDe(t, bd, id); estado != "confirmada" {
		t.Fatalf("se marcó %q a los cinco minutos con un umbral de quince", estado)
	}
}
