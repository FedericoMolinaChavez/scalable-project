package disponibilidad_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// Estas pruebas corren contra la base real. El cálculo entero vive en SQL
// —reglas, menos excepciones, menos reservas, partido en franjas—, así que
// probarlo contra un doble comprobaría el doble y no el cálculo.
//
// El día de este paquete es el miércoles. Ver pruebas.Miercoles: los paquetes
// corren en paralelo sobre el único recurso de la semilla, y estas pruebas
// cuentan franjas, así que otro paquete reservando el mismo día cambiaría el
// recuento por debajo.

func servicio(t *testing.T) (*disponibilidad.Servicio, uuid.UUID, uuid.UUID) {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	return disponibilidad.Nuevo(bd),
		uuid.MustParse(pruebas.Tenant),
		uuid.MustParse(pruebas.Servicio)
}

// La semilla abre de lunes a viernes, de 09:00 a 17:00 hora de Bogotá, con un
// servicio de una hora. Ocho horas de apertura y franjas de sesenta minutos son
// ocho franjas: si salieran nueve, la última pasaría de la hora de cierre.
func TestFranjasDeUnDiaLaborable(t *testing.T) {
	svc, tenant, srv := servicio(t)

	dia := pruebas.Miercoles(0)
	if dia.Weekday() != time.Wednesday {
		t.Fatalf("la fecha ancla de este paquete no es miércoles, es %s", dia.Weekday())
	}

	disp, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}

	if len(disp.Franjas) != 8 {
		t.Fatalf("se esperaban 8 franjas de 09:00 a 17:00, salieron %d", len(disp.Franjas))
	}

	primera := disp.Franjas[0]
	if !primera.Periodo.Inicio.Equal(pruebas.Miercoles(9)) {
		t.Errorf("la primera franja empieza a las %s, se esperaba a las 09:00 locales",
			primera.Periodo.Inicio)
	}

	// El límite superior es abierto: la franja termina cuando empieza la
	// siguiente, y eso es lo que permite que ambas coexistan bajo la
	// restricción EXCLUDE.
	if d := primera.Periodo.Fin.Sub(primera.Periodo.Inicio); d != time.Hour {
		t.Errorf("la franja dura %s, se esperaba la duración del servicio (1 h)", d)
	}
	if !disp.Franjas[1].Periodo.Inicio.Equal(primera.Periodo.Fin) {
		t.Error("la segunda franja no empieza donde acaba la primera; la rejilla tiene un hueco")
	}
}

// El domingo no hay reglas, y "cerrado" tiene que ser una lista vacía y no un
// error: el cliente pinta un calendario y necesita distinguir un día sin
// huecos de un servicio que no existe.
func TestDiaCerradoDevuelveListaVacia(t *testing.T) {
	svc, tenant, srv := servicio(t)

	domingo := pruebas.Domingo(0)

	disp, err := svc.Consultar(t.Context(), tenant, srv, domingo, domingo.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}
	if len(disp.Franjas) != 0 {
		t.Fatalf("el domingo salieron %d franjas y la semilla no abre", len(disp.Franjas))
	}
}

// Y un servicio que no existe SÍ es un error. Es la otra mitad de la prueba
// anterior: si ambos casos devolvieran una lista vacía, el cliente no podría
// distinguir "hoy no hay hueco" de "ese enlace está roto".
func TestServicioInexistenteNoEsListaVacia(t *testing.T) {
	svc, tenant, _ := servicio(t)

	dia := pruebas.Miercoles(0)
	_, err := svc.Consultar(t.Context(), tenant,
		uuid.MustParse("00000000-0000-0000-0000-0000000000aa"),
		dia, dia.Add(24*time.Hour))

	if !errors.Is(err, datos.ErrNoEncontrado) {
		t.Fatalf("se esperaba ErrNoEncontrado, se obtuvo %v", err)
	}
}

// Una pendiente VIGENTE quita su franja; una pendiente VENCIDA no.
//
// Es la prueba que sostiene la coherencia entre leer y escribir. El predicado
// de la restricción EXCLUDE no puede excluir las vencidas —PostgreSQL exige un
// predicado inmutable y now() no lo es—, así que la lectura las descarta y el
// núcleo las recicla dentro de su transacción. Si esta prueba dejara de pasar,
// la disponibilidad y el núcleo estarían viendo cosas distintas: o se ofrecen
// franjas que el motor rechaza, o se esconden franjas que están libres.
func TestPendienteVencidaNoOcupaCupo(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	svc := disponibilidad.Nuevo(bd)
	tenant := uuid.MustParse(pruebas.Tenant)
	srv := uuid.MustParse(pruebas.Servicio)

	inicio, fin := pruebas.Miercoles(11), pruebas.Miercoles(12)
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)

	contar := func() int {
		t.Helper()
		dia := pruebas.Miercoles(0)
		disp, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(24*time.Hour))
		if err != nil {
			t.Fatalf("Consultar devolvió error: %v", err)
		}
		return len(disp.Franjas)
	}

	libresAlPrincipio := contar()

	insertar := func(expiraEn string) {
		t.Helper()
		if err := bd.EnTenant(t.Context(), pruebas.Tenant, func(tx pgx.Tx) error {
			_, err := tx.Exec(t.Context(), `
				INSERT INTO negocio.reserva (
					tenant_id, servicio_id, recurso_id,
					contacto_nombre, contacto_email,
					periodo, estado, expira_en,
					precio_cobrado, moneda, politica_version_id
				) VALUES (
					$1, $2, $3, 'Prueba', 'prueba@ejemplo.test',
					tstzrange($4::timestamptz, $5::timestamptz, '[)'),
					'pendiente', now() + $6::interval,
					80000.00, 'COP', $7
				)`,
				pruebas.Tenant, pruebas.Servicio, pruebas.Recurso,
				inicio, fin, expiraEn, pruebas.Politica)
			return err
		}); err != nil {
			t.Fatalf("no se pudo insertar la reserva de prueba: %v", err)
		}
	}

	insertar("10 minutes")
	if libres := contar(); libres != libresAlPrincipio-1 {
		t.Fatalf("con un bloqueo vigente quedan %d franjas; se esperaban %d",
			libres, libresAlPrincipio-1)
	}

	// La misma fila, ya vencida. Sigue siendo 'pendiente' porque no hay
	// expirador, y aun así el cupo tiene que verse libre.
	pruebas.LimpiarFranja(t, bd, pruebas.Tenant, pruebas.Recurso, inicio, fin)
	insertar("-1 minute")

	if libres := contar(); libres != libresAlPrincipio {
		t.Fatalf("con un bloqueo VENCIDO quedan %d franjas; el cupo debería estar libre y haber %d",
			libres, libresAlPrincipio)
	}
}

// Un día que ya pasó no tiene huecos libres, tiene huecos perdidos. La
// restricción EXCLUDE no lo impide —y no debe: RF-32 registrará asistencias a
// posteriori—, así que lo filtra esta consulta, que es la que sirve a quien
// está eligiendo cita.
func TestElPasadoNoSeOfrece(t *testing.T) {
	svc, tenant, srv := servicio(t)

	// Un lunes de 2020: la semilla abre los lunes, así que sin el filtro del
	// pasado saldrían sus ocho franjas.
	pasado := time.Date(2020, time.January, 6, 0, 0, 0, 0, time.FixedZone("-05", -5*60*60))
	if pasado.Weekday() != time.Monday {
		t.Fatalf("la fecha de la prueba no es lunes, es %s", pasado.Weekday())
	}

	disp, err := svc.Consultar(t.Context(), tenant, srv, pasado, pasado.Add(24*time.Hour))
	if err != nil {
		t.Fatalf("Consultar devolvió error: %v", err)
	}
	if len(disp.Franjas) != 0 {
		t.Fatalf("un día de 2020 ofreció %d franjas libres", len(disp.Franjas))
	}
}

func TestRangosQueNoSeConsultan(t *testing.T) {
	svc, tenant, srv := servicio(t)
	dia := pruebas.Miercoles(0)

	t.Run("invertido", func(t *testing.T) {
		_, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(-time.Hour))
		if !errors.Is(err, disponibilidad.ErrRangoInvalido) {
			t.Fatalf("se esperaba ErrRangoInvalido, se obtuvo %v", err)
		}
	})

	t.Run("ventana excesiva", func(t *testing.T) {
		_, err := svc.Consultar(t.Context(), tenant, srv, dia, dia.Add(disponibilidad.VentanaMaxima+time.Hour))

		var excesiva disponibilidad.ErrVentanaExcesiva
		if !errors.As(err, &excesiva) {
			t.Fatalf("se esperaba ErrVentanaExcesiva, se obtuvo %v", err)
		}
	})
}
