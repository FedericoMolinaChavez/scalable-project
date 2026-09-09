package nucleo_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Lo que cambia cuando quien reserva TIENE cuenta (RF-12 sobre RF-01).
//
// Son tres cosas y las tres van en la misma transacción que la reserva: la fila
// lleva cuenta_id, se escribe plataforma.indice_reserva_global, y la transición
// inicial deja de ser del sistema para tener un actor con nombre.

func cuentaDePrueba(t *testing.T, bd *datos.BD) string {
	t.Helper()

	var id string
	err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO plataforma.cuenta (nombre, email, tipo, estado, email_verificado)
			VALUES ($1, $2, 'usuario', 'activa', true)
			RETURNING id::text`,
			"Núcleo De Prueba",
			"nucleo-cuenta-"+uuid.NewString()[:8]+"@ejemplo.test").Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo crear la cuenta de prueba: %v", err)
	}

	return id
}

// El índice global existe por una razón física: negocio.reserva está
// particionada por HASH(tenant_id), así que "dame mis reservas" no tendría por
// dónde podar y barrería las 64 particiones (RNF-02). Y se escribe DENTRO de la
// transacción: dejarlo para después haría que una reserva recién creada no
// apareciera en el listado de quien acaba de crearla, que es justo la
// lectura-de-lo-escrito que RNF-10 exige.
func TestReservarConCuentaEscribeElIndiceGlobalEnLaMismaTransaccion(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(11), pruebas.Lunes(12)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	cuentaID := cuentaDePrueba(t, bd)

	pet := peticion(clave(t), inicio, fin)
	pet.Cuenta = cuentaID

	reserva, err := svc.Crear(t.Context(), pet)
	if err != nil {
		t.Fatalf("Crear devolvió error: %v", err)
	}

	var conCuenta bool
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT cuenta_id = $2::uuid FROM negocio.reserva WHERE id = $1",
			reserva.Id, cuentaID).Scan(&conCuenta)
	}); err != nil {
		t.Fatalf("no se pudo releer la reserva: %v", err)
	}
	if !conCuenta {
		t.Fatal("la reserva no quedó atada a la cuenta")
	}

	var enElIndice int
	if err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT count(*) FROM plataforma.indice_reserva_global
			WHERE cuenta_id = $1::uuid AND reserva_id = $2`,
			cuentaID, reserva.Id).Scan(&enElIndice)
	}); err != nil {
		t.Fatalf("no se pudo leer el índice global: %v", err)
	}
	if enElIndice != 1 {
		t.Fatalf("el índice global tiene %d filas para esta reserva y debería tener 1", enElIndice)
	}

	// La traza de RF-28 con actor de verdad: RF-36 pregunta quién hizo qué, y
	// "sistema" para todo no puede responderlo.
	var actorTipo string
	var actorID *string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT actor_tipo::text, actor_id::text
			FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_anterior IS NULL`,
			reserva.Id).Scan(&actorTipo, &actorID)
	}); err != nil {
		t.Fatalf("no se pudo leer la transición inicial: %v", err)
	}
	if actorTipo != "usuario" || actorID == nil || *actorID != cuentaID {
		t.Fatalf("la transición dice actor %q/%v y debería decir usuario/%s", actorTipo, actorID, cuentaID)
	}
}

// Una reserva de invitado sigue siendo un caso de primera clase: RF-01 admite
// reservar sin registrarse, y es justo eso lo que crea la necesidad de RF-02.
func TestReservarSinCuentaNoEscribeElIndiceGlobal(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(12), pruebas.Lunes(13)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	reserva, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
	if err != nil {
		t.Fatalf("Crear devolvió error: %v", err)
	}

	var filas int
	if err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT count(*) FROM plataforma.indice_reserva_global WHERE reserva_id = $1",
			reserva.Id).Scan(&filas)
	}); err != nil {
		t.Fatalf("no se pudo leer el índice global: %v", err)
	}
	if filas != 0 {
		t.Fatalf("una reserva de invitado dejó %d filas en el índice de cuentas", filas)
	}

	var actorTipo string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT actor_tipo::text FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_anterior IS NULL`, reserva.Id).Scan(&actorTipo)
	}); err != nil {
		t.Fatalf("no se pudo leer la transición inicial: %v", err)
	}
	if actorTipo != "sistema" {
		t.Fatalf("la transición de un invitado dice %q; transicion_actor_coherente exige "+
			"que solo el sistema tenga actor_id nulo", actorTipo)
	}
}

// El administrador cancela su propia agenda aunque la política ya no lo permita
// (RF-32). La política de RF-15 acota lo que puede hacer un CLIENTE con la
// reserva que compró; el negocio cancela lo suyo cuando le hace falta, y una
// regla que se lo impidiera dejaría la agenda mintiendo sobre lo que va a pasar.
func TestElAdministradorCancelaFueraDePlazo(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)
	tenant := uuid.MustParse(pruebas.Tenant)

	// Dentro del plazo de cancelación de la semilla: una cita de mañana con una
	// política de 24 horas ya no la puede cancelar el cliente.
	inicio := time.Now().Add(2 * time.Hour).Truncate(time.Hour)
	fin := inicio.Add(time.Hour)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	id := reservaSinHorario(t, bd, inicio, fin)
	admin := cuentaDePrueba(t, bd)

	// El cliente choca con la política.
	if _, err := svc.Cancelar(
		t.Context(), tenant, id, dominio.Alcance{Destino: correoPropio}, "",
	); err == nil {
		t.Fatal("el cliente pudo cancelar fuera de plazo")
	}

	// El administrador no.
	// El alcance de un administrador SIEMPRE lleva su cuenta: sale de su token.
	// Sin ella, transicion_actor_coherente rechaza la fila, porque solo el
	// sistema puede quedar sin actor.
	reserva, err := svc.Cancelar(
		t.Context(), tenant, id, dominio.Alcance{TenantCompleto: true, Cuenta: admin}, "")
	if err != nil {
		t.Fatalf("el administrador no pudo cancelar su propia agenda: %v", err)
	}
	if reserva.Estado != "cancelada" {
		t.Fatalf("la reserva quedó en %q", reserva.Estado)
	}

	var actorTipo string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			SELECT actor_tipo::text FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_nuevo = 'cancelada'`, id).Scan(&actorTipo)
	}); err != nil {
		t.Fatalf("no se pudo leer la transición: %v", err)
	}
	if actorTipo != "administrador" {
		t.Fatalf("la cancelación quedó registrada como %q; que cancele el negocio y que "+
			"cancele el cliente son dos hechos distintos, y RF-29 los trata distinto", actorTipo)
	}
}

// reservaSinHorario inserta directamente, saltándose la comprobación de horario
// del núcleo.
//
// Hace falta porque esta prueba necesita una cita CERCANA —dentro del plazo de
// cancelación— y las horas cercanas caen en cualquier día de la semana, no solo
// en los que la semilla abre. Lo que se está probando no es el horario.
func reservaSinHorario(t *testing.T, bd *datos.BD, inicio, fin time.Time) uuid.UUID {
	t.Helper()

	var id uuid.UUID
	err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Prueba del núcleo', $6,
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'confirmada', NULL,
				80000.00, 'COP', $7
			)
			RETURNING id`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, fin, correoPropio, pruebas.Politica).Scan(&id)
	})
	if err != nil {
		t.Fatalf("no se pudo insertar la reserva de prueba: %v", err)
	}

	return id
}
