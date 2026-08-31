package nucleo_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// La ruta crítica, contra la base real. No hay forma honesta de probar esto con
// un doble: lo que se comprueba es que la restricción EXCLUDE rechaza el
// segundo intento, y esa restricción vive en PostgreSQL.

func peticion(clave string, inicio, fin time.Time) nucleo.Peticion {
	return nucleo.Peticion{
		Tenant:            uuid.MustParse(pruebas.Tenant),
		ClaveIdempotencia: clave,
		Nueva: api.NuevaReserva{
			ServicioId: uuid.MustParse(pruebas.Servicio),
			RecursoId:  uuid.MustParse(pruebas.Recurso),
			Periodo:    api.Periodo{Inicio: inicio, Fin: fin},
			Contacto: api.Contacto{
				Nombre: "Prueba del núcleo",
				Email:  openapi_types.Email("nucleo@ejemplo.test"),
			},
		},
	}
}

// clave produce una clave de idempotencia distinta por prueba. Compartirla
// entre pruebas haría que la segunda recibiera la reserva de la primera y
// pasara sin comprobar nada.
func clave(t *testing.T) string {
	t.Helper()
	return "prb-" + uuid.NewString()
}

func TestCrearDevuelvePendienteConCupoYPrecioCongelado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(9), pruebas.Lunes(10)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	reserva, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
	if err != nil {
		t.Fatalf("Crear devolvió error: %v", err)
	}

	if reserva.Estado != api.Pendiente {
		t.Errorf("estado %q; la reserva nace pendiente y el pago la confirma después (RF-33)", reserva.Estado)
	}

	// Una pendiente sin vencimiento es una denegación de inventario permanente:
	// el cupo quedaría bloqueado para siempre sin que nadie pagara.
	if reserva.ExpiraEn == nil {
		t.Fatal("la reserva pendiente no trae expira_en; el bloqueo de RF-27 nunca vencería")
	}
	if !reserva.ExpiraEn.After(time.Now()) {
		t.Errorf("expira_en (%s) ya pasó al crearse", reserva.ExpiraEn)
	}

	// El precio se copia del catálogo al crear (RF-31). Si viniera vacío o a
	// cero, un cambio de tarifa posterior alcanzaría a esta reserva.
	if reserva.PrecioCobrado.Monto != "80000.00" {
		t.Errorf("precio cobrado %q; se esperaba el del catálogo sembrado", reserva.PrecioCobrado.Monto)
	}
	// La moneda no está en negocio.servicio: sale de plataforma.tenant.
	if reserva.PrecioCobrado.Moneda != "COP" {
		t.Errorf("moneda %q; se esperaba la del tenant", reserva.PrecioCobrado.Moneda)
	}
}

// El caso que justifica toda la arquitectura: dos peticiones por el mismo cupo,
// una gana. No se comprueba disponibilidad y luego se inserta —eso sería una
// carrera con una ventana entre las dos consultas—: se inserta y se traduce el
// rechazo del motor.
func TestSegundaReservaSobreElMismoCupoDaHorarioOcupado(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(10), pruebas.Lunes(11)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	if _, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin)); err != nil {
		t.Fatalf("la primera reserva debía funcionar: %v", err)
	}

	// Clave DISTINTA: es otra operación pidiendo el mismo horario, no un
	// reintento de la primera.
	_, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
	if !errors.Is(err, datos.ErrHorarioOcupado) {
		t.Fatalf("se esperaba ErrHorarioOcupado, se obtuvo %v (SQLSTATE %q)", err, datos.CodigoPG(err))
	}
	if codigo := datos.CodigoPG(err); codigo != "23P01" {
		t.Errorf("SQLSTATE %q bajo el error traducido; se esperaba 23P01 (exclusion_violation)", codigo)
	}
}

// El reintento de un agente tras un timeout. Sin idempotencia crearía un
// segundo bloqueo sobre otro horario que nadie libera hasta el TTL: denegación
// de inventario por accidente, no por ataque.
func TestMismaClaveDevuelveLaMismaReservaYNoCreaOtra(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(11), pruebas.Lunes(12)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	repetida := clave(t)

	primera, err := svc.Crear(t.Context(), peticion(repetida, inicio, fin))
	if err != nil {
		t.Fatalf("la primera reserva debía funcionar: %v", err)
	}

	segunda, err := svc.Crear(t.Context(), peticion(repetida, inicio, fin))
	if err != nil {
		t.Fatalf("el reintento debía devolver la reserva ya creada: %v", err)
	}

	if primera.Id != segunda.Id {
		t.Fatalf("el reintento devolvió otra reserva (%s vs %s)", primera.Id, segunda.Id)
	}

	var filas int
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(),
			"SELECT count(*) FROM negocio.reserva WHERE clave_idempotencia = $1", repetida).Scan(&filas)
	}); err != nil {
		t.Fatalf("no se pudo contar: %v", err)
	}
	if filas != 1 {
		t.Fatalf("quedaron %d filas con la misma clave de idempotencia", filas)
	}
}

// Un bloqueo vencido no debe secuestrar el cupo. Sin expirador (RF-27), la
// restricción EXCLUDE seguiría contándolo, así que el núcleo lo recicla dentro
// de la misma transacción que decide. Es la contrapartida de que la
// disponibilidad ya lo muestre libre.
func TestBloqueoVencidoSeReciclaYDejaReservar(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	inicio, fin := pruebas.Lunes(12), pruebas.Lunes(13)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	// Un bloqueo que ya venció, tal como lo dejaría un checkout abandonado.
	var vencidaID string
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		return tx.QueryRow(t.Context(), `
			INSERT INTO negocio.reserva (
				tenant_id, servicio_id, recurso_id,
				contacto_nombre, contacto_email,
				periodo, estado, expira_en,
				precio_cobrado, moneda, politica_version_id
			) VALUES (
				$1, $2, $3, 'Abandonada', 'abandonada@ejemplo.test',
				tstzrange($4::timestamptz, $5::timestamptz, '[)'),
				'pendiente', now() - interval '1 minute',
				80000.00, 'COP', $6
			)
			RETURNING id::text`,
			pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
			inicio, fin, pruebas.Politica).Scan(&vencidaID)
	}); err != nil {
		t.Fatalf("no se pudo sembrar el bloqueo vencido: %v", err)
	}

	if _, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin)); err != nil {
		t.Fatalf("el bloqueo vencido siguió ocupando el cupo: %v", err)
	}

	// El reciclado deja rastro. Un cambio de estado sin transición rompe RF-28:
	// la reserva aparecería expirada sin que nada explique quién la expiró.
	var estado string
	var transiciones int
	if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
		if err := tx.QueryRow(t.Context(),
			"SELECT estado::text FROM negocio.reserva WHERE id = $1", vencidaID).Scan(&estado); err != nil {
			return err
		}
		return tx.QueryRow(t.Context(), `
			SELECT count(*) FROM negocio.transicion_estado
			WHERE reserva_id = $1 AND estado_nuevo = 'expirada'`, vencidaID).Scan(&transiciones)
	}); err != nil {
		t.Fatalf("no se pudo verificar el reciclado: %v", err)
	}

	if estado != "expirada" {
		t.Errorf("el bloqueo vencido quedó en %q; se esperaba 'expirada'", estado)
	}
	if transiciones != 1 {
		t.Errorf("se escribieron %d transiciones a 'expirada'; se esperaba 1", transiciones)
	}
}

// Las reglas que el motor NO hace cumplir. Cada una existe porque ninguna
// restricción del esquema puede expresarla: todas exigen mirar otra tabla.
func TestReglasQueElMotorNoCubre(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	t.Run("duración distinta a la del servicio", func(t *testing.T) {
		inicio, fin := pruebas.Lunes(14), pruebas.Lunes(16)
		pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

		_, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
		if !errors.Is(err, dominio.ErrDuracionNoCoincide) {
			t.Fatalf("se esperaba ErrDuracionNoCoincide, se obtuvo %v", err)
		}
	})

	t.Run("fuera del horario del recurso", func(t *testing.T) {
		// Las 03:00 de un lunes: dentro de un día que abre, fuera de su franja.
		inicio, fin := pruebas.Lunes(3), pruebas.Lunes(4)

		_, err := svc.Crear(t.Context(), peticion(clave(t), inicio, fin))
		if !errors.Is(err, dominio.ErrFueraDeHorario) {
			t.Fatalf("se esperaba ErrFueraDeHorario, se obtuvo %v", err)
		}
	})

	t.Run("recurso que no presta el servicio", func(t *testing.T) {
		pet := peticion(clave(t), pruebas.Lunes(9), pruebas.Lunes(10))
		pet.Nueva.RecursoId = uuid.MustParse("00000000-0000-0000-0000-0000000000bb")

		_, err := svc.Crear(t.Context(), pet)
		if !errors.Is(err, dominio.ErrRecursoNoPresta) {
			t.Fatalf("se esperaba ErrRecursoNoPresta, se obtuvo %v", err)
		}
	})

	t.Run("servicio inexistente", func(t *testing.T) {
		pet := peticion(clave(t), pruebas.Lunes(9), pruebas.Lunes(10))
		pet.Nueva.ServicioId = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

		_, err := svc.Crear(t.Context(), pet)
		if !errors.Is(err, datos.ErrNoEncontrado) {
			t.Fatalf("se esperaba ErrNoEncontrado, se obtuvo %v", err)
		}
	})

	// El motor acepta una reserva de ayer sin protestar, y hace bien: RF-32
	// necesita registrar asistencias a posteriori. Lo que no puede es llegar
	// por la ruta del cliente, donde reservar el pasado no significa nada.
	t.Run("cita en el pasado", func(t *testing.T) {
		ayer := time.Now().Add(-24 * time.Hour)

		_, err := svc.Crear(t.Context(), peticion(clave(t), ayer, ayer.Add(time.Hour)))
		if !errors.Is(err, dominio.ErrPeriodoEnElPasado) {
			t.Fatalf("se esperaba ErrPeriodoEnElPasado, se obtuvo %v", err)
		}
	})

	t.Run("período invertido", func(t *testing.T) {
		_, err := svc.Crear(t.Context(), peticion(clave(t), pruebas.Lunes(10), pruebas.Lunes(9)))
		if !errors.Is(err, dominio.ErrPeriodoInvalido) {
			t.Fatalf("se esperaba ErrPeriodoInvalido, se obtuvo %v", err)
		}
	})

	t.Run("clave de idempotencia demasiado corta", func(t *testing.T) {
		pet := peticion("corta", pruebas.Lunes(9), pruebas.Lunes(10))

		_, err := svc.Crear(t.Context(), pet)
		if !errors.Is(err, nucleo.ErrClaveIdempotenciaCorta) {
			t.Fatalf("se esperaba ErrClaveIdempotenciaCorta, se obtuvo %v", err)
		}
	})

	t.Run("voucher, que esta rebanada no aplica", func(t *testing.T) {
		pet := peticion(clave(t), pruebas.Lunes(9), pruebas.Lunes(10))
		codigo := "DESC20"
		pet.Nueva.VoucherCodigo = &codigo

		_, err := svc.Crear(t.Context(), pet)
		if !errors.Is(err, nucleo.ErrVoucherNoSoportado) {
			t.Fatalf("se esperaba ErrVoucherNoSoportado, se obtuvo %v", err)
		}
	})
}

// El aislamiento entre tenants no es cosa del código: la clave foránea compuesta
// arrastra tenant_id, así que reservar el recurso de otro tenant es una fila que
// el motor rechaza, no un caso que haya que acordarse de comprobar.
func TestNoSePuedeReservarElRecursoDeOtroTenant(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := nucleo.Nuevo(bd, 15*time.Minute)

	pet := peticion(clave(t), pruebas.Lunes(9), pruebas.Lunes(10))
	pet.Tenant = uuid.MustParse(pruebas.TenantAjeno)

	// Bajo el contexto del tenant B, el servicio de A no existe. Es la
	// respuesta correcta: distinguir "no existe" de "existe pero es ajeno"
	// filtraría la existencia de datos de otro negocio.
	_, err := svc.Crear(t.Context(), pet)
	if !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se esperaba ErrNoEncontrado, se obtuvo %v", err)
	}
}
