package datos

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Los errores que el motor hace cumplir, traducidos a algo sobre lo que el
// dominio pueda decidir.
//
// Esto no es cosmética. Todo el diseño apuesta a que la invariante viva en
// PostgreSQL y no en Go: la restricción EXCLUDE es lo único que sigue siendo
// cierto con 45 pods escribiendo sin coordinarse (db/README.md). La
// consecuencia es que el camino normal de "el horario ya estaba tomado" es un
// error del motor, no una comprobación previa. Comprobar antes y luego
// insertar sería una carrera; insertar y traducir el rechazo, no.
var (
	// ErrHorarioOcupado corresponde a 23P01, exclusion_violation. Es la
	// invariante de RNF-10 rechazando un solapamiento: otra transacción se
	// quedó con el cupo.
	ErrHorarioOcupado = errors.New("el horario ya está reservado")

	// ErrDuplicado corresponde a 23505, unique_violation.
	ErrDuplicado = errors.New("ya existe un registro con esa clave")

	// ErrReferenciaInvalida corresponde a 23503, foreign_key_violation. Con
	// claves foráneas compuestas por (tenant_id, x_id), esto incluye el caso
	// de referenciar una fila de otro tenant: no es un error a evitar, es una
	// fila que el motor rechaza.
	ErrReferenciaInvalida = errors.New("referencia inexistente o de otro tenant")

	// ErrRestriccion corresponde a 23514, check_violation.
	ErrRestriccion = errors.New("la fila viola una restricción del esquema")

	// ErrNoEncontrado unifica pgx.ErrNoRows. Ojo: bajo RLS, "no encontrado" y
	// "existe pero es de otro tenant" son indistinguibles desde el código, y
	// así debe ser.
	ErrNoEncontrado = errors.New("no encontrado")
)

// CodigoPG expone el código SQLSTATE de un error del motor, o cadena vacía si
// el error no viene de PostgreSQL. Útil para registrar el caso concreto sin
// obligar al dominio a conocer los códigos.
func CodigoPG(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// RestriccionPG expone el nombre de la restricción que rechazó la fila, o
// cadena vacía si el error no viene de PostgreSQL.
//
// Compañero de CodigoPG y por el mismo motivo: el código dice QUÉ clase de
// violación fue, y el nombre dice CUÁL. Distinguirlas importa en cuanto una
// tabla tiene dos restricciones del mismo código —dos índices únicos, por
// ejemplo—, porque tratarlas igual convierte un caso en el otro sin avisar.
func RestriccionPG(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// traducir convierte un error de pgx en uno de este paquete, conservando el
// original con %w para que errors.As siga alcanzando al *pgconn.PgError.
func traducir(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNoEncontrado
	}

	// errors.As y no una comparación de cadenas: el mensaje del motor cambia
	// entre versiones y viene traducido según el locale del servidor. El
	// SQLSTATE no.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}

	// Se envuelven los DOS errores, no solo el centinela. Con un único %w el
	// dominio podría preguntar errors.Is(err, ErrHorarioOcupado) pero se
	// perdería el *pgconn.PgError, y con él la restricción concreta que
	// rechazó la fila, el detalle y la tabla: justo lo que hace falta para
	// diagnosticar en producción. Envolviendo ambos, errors.Is alcanza al
	// centinela y errors.As al error del motor.
	switch pgErr.Code {
	case "23P01":
		return fmt.Errorf("%w (%s): %w", ErrHorarioOcupado, pgErr.ConstraintName, err)
	case "23505":
		return fmt.Errorf("%w (%s): %w", ErrDuplicado, pgErr.ConstraintName, err)
	case "23503":
		return fmt.Errorf("%w (%s): %w", ErrReferenciaInvalida, pgErr.ConstraintName, err)
	case "23514":
		return fmt.Errorf("%w (%s): %w", ErrRestriccion, pgErr.ConstraintName, err)
	default:
		return err
	}
}
