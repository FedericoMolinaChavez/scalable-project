// Package nucleo es el componente "Núcleo de Reservas" de ARQ-01: el único que
// escribe en negocio.reserva por la ruta síncrona, y por tanto el dueño de la
// invariante de RNF-10.
//
// Todo lo que hace ocurre en UNA transacción. Esa es la razón por la que este
// componente existe separado y no hay un "servicio de bloqueos" ni un "servicio
// de pagos" síncrono al lado: partirlos obligaría a una transacción distribuida,
// y una invariante que necesita dos confirmaciones para sostenerse no es una
// invariante.
package nucleo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/reservas"
)

// restriccionIdempotencia es el índice único que arbitra los reintentos. Se
// nombra para poder distinguir SU 23505 del de cualquier otra restricción: dar
// por hecho que todo duplicado viene de aquí devolvería una reserva ajena el
// día que exista una segunda clave única sobre la tabla.
const restriccionIdempotencia = "reserva_idempotencia_uq"

// Errores propios de esta ruta.
var (
	// ErrSinPoliticaVigente: el tenant no tiene ninguna política de cancelación
	// publicada. reserva.politica_version_id es NOT NULL, así que sin política
	// no se puede reservar. Es una configuración incompleta del negocio, no un
	// fallo del servidor, y se responde como tal.
	ErrSinPoliticaVigente = errors.New("el negocio no tiene una política de cancelación vigente")

	// ErrVoucherNoSoportado: el contrato ya acepta voucher_codigo, pero esta
	// rebanada no consume vouchers. Se rechaza explícitamente en vez de
	// ignorar el campo: aceptar un código y cobrar el precio completo sin
	// decirlo es peor que negarse.
	ErrVoucherNoSoportado = errors.New("los vouchers todavía no se aplican al crear una reserva")

	// ErrClaveIdempotenciaCorta: el contrato exige entre 8 y 128 caracteres.
	// Una clave corta es una clave que colisiona, y una colisión aquí devuelve
	// la reserva de otra persona.
	ErrClaveIdempotenciaCorta = errors.New("la clave de idempotencia debe tener entre 8 y 128 caracteres")
)

// TTLPorDefecto es cuánto vive un bloqueo antes de vencer (RF-27).
//
// RF-27 pide "un TTL definido" sin fijar el número, así que lo fija la
// configuración. El valor por defecto sale del uso: es el tiempo de un
// checkout con pago, no un margen técnico. Más corto expulsa a quien busca la
// tarjeta; más largo retiene inventario que nadie va a comprar.
const TTLPorDefecto = 15 * time.Minute

// Servicio crea reservas.
type Servicio struct {
	bd  *datos.BD
	ttl time.Duration
}

func Nuevo(bd *datos.BD, ttl time.Duration) *Servicio {
	if ttl <= 0 {
		ttl = TTLPorDefecto
	}
	return &Servicio{bd: bd, ttl: ttl}
}

// Peticion es lo que hace falta para crear una reserva, ya extraído del HTTP.
type Peticion struct {
	Tenant            uuid.UUID
	ClaveIdempotencia string
	Nueva             api.NuevaReserva
}

// Crear inserta la reserva pendiente con el cupo ya garantizado.
//
// Devuelve datos.ErrHorarioOcupado cuando la restricción EXCLUDE rechaza la
// fila. Ese es el camino normal de perder una carrera, no una excepción: el
// núcleo NO comprueba disponibilidad y luego inserta —eso sería una carrera con
// una ventana entre las dos consultas—, inserta y traduce el rechazo del motor.
func (s *Servicio) Crear(ctx context.Context, pet Peticion) (api.Reserva, error) {
	if n := len(pet.ClaveIdempotencia); n < 8 || n > 128 {
		return api.Reserva{}, ErrClaveIdempotenciaCorta
	}
	if pet.Nueva.VoucherCodigo != nil && *pet.Nueva.VoucherCodigo != "" {
		return api.Reserva{}, ErrVoucherNoSoportado
	}

	periodo := dominio.Periodo{Inicio: pet.Nueva.Periodo.Inicio, Fin: pet.Nueva.Periodo.Fin}
	if !periodo.Valido() {
		return api.Reserva{}, dominio.ErrPeriodoInvalido
	}

	// Se compara contra el reloj de este proceso y no contra el del motor. La
	// diferencia entre ambos es de milisegundos y esto no es una invariante: es
	// una comprobación de sensatez sobre lo que pide un cliente. Las
	// invariantes —el solapamiento— siguen siendo del motor.
	if !periodo.Inicio.After(time.Now()) {
		return api.Reserva{}, dominio.ErrPeriodoEnElPasado
	}

	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, pet.Tenant.String(), func(tx pgx.Tx) error {
		var err error
		reserva, err = s.crearEnTx(ctx, tx, pet, periodo)
		return err
	})

	// La clave de idempotencia ya existía. La transacción abortó al chocar con
	// reserva_idempotencia_uq, así que la lectura no cabe dentro de ella y va
	// en una nueva: es el precio de dejar que el índice único arbitre en vez de
	// consultar antes de insertar, y a cambio dos peticiones simultáneas con la
	// misma clave no pueden crear dos reservas.
	if errors.Is(err, datos.ErrDuplicado) && datos.RestriccionPG(err) == restriccionIdempotencia {
		return s.porClave(ctx, pet.Tenant, pet.ClaveIdempotencia)
	}
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}

func (s *Servicio) crearEnTx(
	ctx context.Context, tx pgx.Tx, pet Peticion, periodo dominio.Periodo,
) (api.Reserva, error) {
	// Camino rápido de idempotencia: si la clave ya se usó, esta petición es un
	// reintento y devuelve lo mismo que la primera. Sin esto, un agente que
	// reintenta tras un timeout crea un segundo bloqueo sobre otro horario que
	// nadie libera hasta el TTL: denegación de inventario por accidente.
	if existente, encontrada, err := leerPorClave(ctx, tx, pet.ClaveIdempotencia); err != nil {
		return api.Reserva{}, err
	} else if encontrada {
		return existente, nil
	}

	// Reciclado de bloqueos vencidos, acotado a este recurso y a este período.
	//
	// No sustituye al expirador de RF-27, que barre la tabla entera y no existe
	// todavía: cubre el caso concreto que está a punto de estorbar. Hace falta
	// porque el predicado de la restricción EXCLUDE no puede excluir las
	// pendientes vencidas —PostgreSQL exige un predicado inmutable y now() no
	// lo es—, así que una pendiente muerta seguiría bloqueando el cupo aunque
	// la disponibilidad ya lo muestre libre. Aquí las dos vistas se reconcilian
	// dentro de la misma transacción que decide.
	if err := reciclarVencidas(ctx, tx, pet.Tenant, pet.Nueva.RecursoId, periodo); err != nil {
		return api.Reserva{}, err
	}

	oferta, err := leerOferta(ctx, tx, pet.Nueva.ServicioId, pet.Nueva.RecursoId)
	if err != nil {
		return api.Reserva{}, err
	}
	if !oferta.recursoActivo || !oferta.presta {
		return api.Reserva{}, dominio.ErrRecursoNoPresta
	}
	if !periodo.CoincideCon(oferta.duracionMin) {
		return api.Reserva{}, dominio.ErrDuracionNoCoincide
	}

	// El horario del recurso. La restricción EXCLUDE solo impide que dos
	// reservas se pisen; sin esta comprobación se podría reservar a las tres de
	// la mañana de un domingo cerrado, porque a esa hora no hay nada con lo que
	// chocar.
	abierto, err := estaEnHorario(ctx, tx, pet.Nueva.RecursoId, periodo)
	if err != nil {
		return api.Reserva{}, err
	}
	if !abierto {
		return api.Reserva{}, dominio.ErrFueraDeHorario
	}

	politica, err := politicaVigente(ctx, tx, pet.Nueva.ServicioId)
	if err != nil {
		return api.Reserva{}, err
	}

	return insertar(ctx, tx, pet, periodo, oferta, politica, s.ttl)
}

// oferta es lo que el catálogo dice de este par servicio/recurso, congelado
// para el resto de la transacción.
type oferta struct {
	duracionMin   int
	precio        string
	moneda        string
	presta        bool
	recursoActivo bool
}

func leerOferta(ctx context.Context, tx pgx.Tx, servicio, recurso uuid.UUID) (oferta, error) {
	var o oferta

	// Una sola ida a la base para las cuatro respuestas. Son cuatro consultas
	// triviales, pero están en la ruta de los 200 ms de RNF-01 y cada viaje
	// atraviesa PgBouncer.
	err := tx.QueryRow(ctx, `
		SELECT s.duracion_min,
		       s.precio_monto::text,
		       t.moneda,
		       EXISTS (SELECT 1 FROM negocio.servicio_recurso sr
		               WHERE sr.servicio_id = s.id AND sr.recurso_id = $2),
		       EXISTS (SELECT 1 FROM negocio.recurso r
		               WHERE r.id = $2 AND r.estado = 'activo')
		FROM negocio.servicio s
		JOIN plataforma.tenant t ON t.id = s.tenant_id
		WHERE s.id = $1 AND s.estado = 'activo'`,
		servicio, recurso,
	).Scan(&o.duracionMin, &o.precio, &o.moneda, &o.presta, &o.recursoActivo)
	if err != nil {
		return oferta{}, err
	}

	return o, nil
}

// estaEnHorario comprueba que el período completo cabe dentro de una regla
// vigente del recurso y que ninguna excepción lo tapa.
//
// La regla se evalúa en el día LOCAL de la sede, no en UTC: regla_disponibilidad
// guarda `time` sin zona porque "los lunes de 9 a 17" habla del reloj de pared
// del negocio. Una regla no cruza medianoche —lo prohíbe regla_franja_valida—,
// así que basta con mirar el día local en que empieza el período.
func estaEnHorario(ctx context.Context, tx pgx.Tx, recurso uuid.UUID, p dominio.Periodo) (bool, error) {
	var dentro, tapado bool

	err := tx.QueryRow(ctx, `
		WITH recurso AS (
			SELECT r.id, r.sede_id, sd.zona_horaria
			FROM negocio.recurso r
			JOIN negocio.sede sd ON sd.tenant_id = r.tenant_id AND sd.id = r.sede_id
			WHERE r.id = $1
		),
		local AS (
			SELECT rec.id, rec.sede_id, rec.zona_horaria,
			       ($2::timestamptz AT TIME ZONE rec.zona_horaria)::date AS dia
			FROM recurso rec
		)
		SELECT
			EXISTS (
				SELECT 1
				FROM negocio.regla_disponibilidad reg
				JOIN local l ON l.id = reg.recurso_id
				WHERE reg.dia_semana = extract(dow FROM l.dia)
				  AND (reg.vigente_desde IS NULL OR l.dia >= reg.vigente_desde)
				  AND (reg.vigente_hasta IS NULL OR l.dia <= reg.vigente_hasta)
				  AND tstzrange(
				        (l.dia + reg.hora_inicio) AT TIME ZONE l.zona_horaria,
				        (l.dia + reg.hora_fin)    AT TIME ZONE l.zona_horaria, '[)'
				      ) @> tstzrange($2::timestamptz, $3::timestamptz, '[)')
			),
			EXISTS (
				SELECT 1
				FROM negocio.excepcion_calendario e, local l
				WHERE e.periodo && tstzrange($2::timestamptz, $3::timestamptz, '[)')
				  AND (e.recurso_id = l.id OR e.sede_id = l.sede_id)
			)`,
		recurso, p.Inicio, p.Fin,
	).Scan(&dentro, &tapado)
	if err != nil {
		return false, err
	}

	return dentro && !tapado, nil
}

// politicaVigente resuelve qué versión de política se congela en esta reserva.
//
// Gana la específica del servicio sobre la del tenant, y entre varias, la más
// reciente. La reserva guarda ese identificador para siempre: es lo que impide
// que un negocio endurezca su política el martes y se la aplique a quien
// reservó el lunes (RF-15).
func politicaVigente(ctx context.Context, tx pgx.Tx, servicio uuid.UUID) (string, error) {
	var id string

	err := tx.QueryRow(ctx, `
		SELECT id::text
		FROM negocio.politica_version
		WHERE (servicio_id = $1 OR servicio_id IS NULL)
		  AND vigente_desde <= now()
		ORDER BY (servicio_id IS NULL), vigente_desde DESC, version DESC
		LIMIT 1`, servicio).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSinPoliticaVigente
	}
	if err != nil {
		return "", err
	}

	return id, nil
}

func reciclarVencidas(
	ctx context.Context, tx pgx.Tx, tenant, recurso uuid.UUID, p dominio.Periodo,
) error {
	filas, err := tx.Query(ctx, `
		UPDATE negocio.reserva
		SET estado = 'expirada'
		WHERE recurso_id = $1
		  AND estado = 'pendiente'
		  AND expira_en <= now()
		  AND periodo && tstzrange($2::timestamptz, $3::timestamptz, '[)')
		RETURNING id::text`,
		recurso, p.Inicio, p.Fin)
	if err != nil {
		return err
	}

	var vencidas []string
	for filas.Next() {
		var id string
		if err := filas.Scan(&id); err != nil {
			filas.Close()
			return err
		}
		vencidas = append(vencidas, id)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return err
	}

	// El historial de RF-28 se escribe en la misma transacción que el cambio.
	// Si se dejara para después, un fallo entre ambos dejaría una reserva
	// expirada sin explicación de quién la expiró.
	for _, id := range vencidas {
		if _, err := tx.Exec(ctx, `
			INSERT INTO negocio.transicion_estado
				(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
			VALUES ($1, $2, 'pendiente', 'expirada', 'sistema', $3)`,
			tenant, id, "el bloqueo venció sin pago (RF-27)"); err != nil {
			return err
		}
	}

	return nil
}

func insertar(
	ctx context.Context, tx pgx.Tx, pet Peticion, p dominio.Periodo,
	o oferta, politica string, ttl time.Duration,
) (api.Reserva, error) {
	var (
		reserva api.Reserva
		id      string
		estado  string
	)

	// cuenta_id va en NULL: sin autenticación (RF-12) toda reserva es de
	// invitado, y por eso reserva_contacto_requerido exige nombre y correo.
	// Cuando exista el token, el identificador de la cuenta sale de ahí y hay
	// que escribir además plataforma.indice_reserva_global.
	err := tx.QueryRow(ctx, `
		INSERT INTO negocio.reserva (
			tenant_id, servicio_id, recurso_id,
			contacto_nombre, contacto_email, contacto_telefono,
			periodo, estado, expira_en,
			precio_cobrado, moneda, politica_version_id, clave_idempotencia
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			tstzrange($7::timestamptz, $8::timestamptz, '[)'),
			'pendiente', now() + make_interval(secs => $9),
			$10::numeric, $11, $12, $13
		)
		RETURNING id::text, estado::text, creada_en, expira_en, precio_cobrado::text`,
		pet.Tenant, pet.Nueva.ServicioId, pet.Nueva.RecursoId,
		pet.Nueva.Contacto.Nombre, string(pet.Nueva.Contacto.Email), pet.Nueva.Contacto.Telefono,
		p.Inicio, p.Fin, ttl.Seconds(),
		o.precio, o.moneda, politica, pet.ClaveIdempotencia,
	).Scan(&id, &estado, &reserva.CreadaEn, &reserva.ExpiraEn, &reserva.PrecioCobrado.Monto)
	if err != nil {
		return api.Reserva{}, err
	}

	reservaID, err := uuid.Parse(id)
	if err != nil {
		return api.Reserva{}, fmt.Errorf("el motor devolvió un id ilegible: %w", err)
	}

	// Primera transición: de nada a pendiente. actor_tipo es 'sistema' porque
	// transicion_actor_coherente exige que solo el sistema tenga actor_id nulo,
	// y sin RF-12 no hay identidad que poner. Cuando exista, esto pasa a
	// 'usuario' con su cuenta detrás.
	if _, err := tx.Exec(ctx, `
		INSERT INTO negocio.transicion_estado
			(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, motivo)
		VALUES ($1, $2, NULL, 'pendiente', 'sistema', $3)`,
		pet.Tenant, id, "creación de la reserva (RF-01)"); err != nil {
		return api.Reserva{}, err
	}

	reserva.Id = reservaID
	reserva.ServicioId = pet.Nueva.ServicioId
	reserva.RecursoId = pet.Nueva.RecursoId
	reserva.Estado = api.EstadoReserva(estado)
	reserva.Periodo = api.Periodo{Inicio: p.Inicio, Fin: p.Fin}
	reserva.PrecioCobrado.Moneda = o.moneda
	contacto := pet.Nueva.Contacto
	reserva.Contacto = &contacto

	return reserva, nil
}

// porClave relee una reserva ya creada con esta clave de idempotencia.
func (s *Servicio) porClave(ctx context.Context, tenant uuid.UUID, clave string) (api.Reserva, error) {
	var reserva api.Reserva

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		existente, encontrada, err := leerPorClave(ctx, tx, clave)
		if err != nil {
			return err
		}
		if !encontrada {
			// La fila que acaba de provocar el conflicto no está: solo puede
			// ser de otro tenant, y el índice único no distingue tenants
			// porque incluye tenant_id. Si esto ocurre, hay un fallo real que
			// merece verse como tal y no como un 404.
			return fmt.Errorf("clave de idempotencia en conflicto sin fila visible: %w", datos.ErrDuplicado)
		}
		reserva = existente
		return nil
	})
	if err != nil {
		return api.Reserva{}, err
	}

	return reserva, nil
}

func leerPorClave(ctx context.Context, tx pgx.Tx, clave string) (api.Reserva, bool, error) {
	reserva, err := reservas.Escanear(tx.QueryRow(ctx,
		`SELECT `+reservas.Columnas+`
		 FROM negocio.reserva
		 WHERE clave_idempotencia = $1`, clave))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.Reserva{}, false, nil
	}
	if err != nil {
		return api.Reserva{}, false, err
	}

	return reserva, true, nil
}
