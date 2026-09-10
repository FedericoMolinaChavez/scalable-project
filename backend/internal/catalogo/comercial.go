package catalogo

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Lo que altera el precio: vouchers (RF-17) y tarifas (RF-31).
//
// Las dos comparten una propiedad que conviene tener presente: nada de lo que
// se hace aquí toca una reserva existente. La reserva congela precio_cobrado y
// moneda al crearse, así que publicar una tarifa o retirar un voucher cambia lo
// que costará la próxima, nunca lo que ya se cobró.

// -------------------------------------------------------- vouchers (RF-17) --

// Vouchers devuelve los del tenant, incluidos los eliminados.
//
// Los eliminados también, y a propósito: su código no se libera —reutilizarlo
// dejaría dos promociones distintas con el mismo código en el historial— así
// que quien crea uno nuevo necesita ver cuáles ya se gastaron para no chocar.
func (s *Servicio) Vouchers(ctx context.Context, tenant uuid.UUID) ([]api.Voucher, error) {
	vouchers := make([]api.Voucher, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, codigo, porcentaje::text, limite_usos, usos_actuales,
			       caduca_en, estado::text, creado_en
			FROM negocio.voucher
			ORDER BY creado_en DESC`)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			voucher, err := escanearVoucher(filas)
			if err != nil {
				return err
			}
			vouchers = append(vouchers, voucher)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return vouchers, nil
}

// CrearVoucher da de alta un descuento (RF-17).
//
// No hay operación de edición, y no es un hueco: RF-17 admite crear o eliminar
// y nada más. Cambiarle las condiciones a un voucher que ya está circulando es
// cambiárselas a quien ya lo recibió, y el motor lo impide revocando el UPDATE
// sobre todas las columnas menos el contador de usos.
func (s *Servicio) CrearVoucher(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nuevo api.NuevoVoucher,
) (api.Voucher, error) {
	var voucher api.Voucher

	_, err := s.enTx(ctx, tenant, actor, "crear_voucher", auditoria.RecursoVoucher,
		func(tx pgx.Tx) (string, error) {
			// El CHECK voucher_tiene_limite exige límite de usos o caducidad:
			// un voucher del 50% sin ninguno de los dos es una fuga de ingresos
			// abierta para siempre. Se deja decidir al motor en vez de
			// comprobarlo aquí, porque es una regla del esquema y hay más de
			// una ruta que podría escribir esta tabla.
			var err error
			voucher, err = escanearVoucher(tx.QueryRow(ctx, `
				INSERT INTO negocio.voucher
					(tenant_id, codigo, porcentaje, limite_usos, caduca_en)
				VALUES ($1, $2, $3::numeric, $4, $5)
				RETURNING id::text, codigo, porcentaje::text, limite_usos, usos_actuales,
				          caduca_en, estado::text, creado_en`,
				tenant, nuevo.Codigo, nuevo.Porcentaje, nuevo.LimiteUsos, nuevo.CaducaEn))
			if err != nil {
				return "", err
			}
			return voucher.Id.String(), nil
		})
	if err != nil {
		return api.Voucher{}, err
	}

	return voucher, nil
}

// EliminarVoucher lo retira de circulación (RF-17).
//
// Borrado lógico: la fila permanece porque uso_voucher la referencia, y con
// ella la evidencia de cuánto se usó y en qué reservas.
func (s *Servicio) EliminarVoucher(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, id uuid.UUID,
) error {
	_, err := s.enTx(ctx, tenant, actor, "eliminar_voucher", auditoria.RecursoVoucher,
		func(tx pgx.Tx) (string, error) {
			// El rol de la aplicación solo tiene UPDATE sobre (usos_actuales,
			// estado): la migración 0007 revoca el resto para que RF-17 no
			// dependa de que nadie escriba el UPDATE equivocado.
			etiqueta, err := tx.Exec(ctx, `
				UPDATE negocio.voucher
				SET estado = 'eliminado'
				WHERE id = $1 AND estado = 'activo'`, id)
			if err != nil {
				return id.String(), err
			}
			if etiqueta.RowsAffected() == 0 {
				return id.String(), datos.ErrNoEncontrado
			}
			return id.String(), nil
		})
	return err
}

// --------------------------------------------------------- tarifas (RF-31) --

// Tarifas devuelve las del tenant, de mayor a menor prioridad.
func (s *Servicio) Tarifas(
	ctx context.Context, tenant uuid.UUID, servicio *uuid.UUID,
) ([]api.Tarifa, error) {
	tarifas := make([]api.Tarifa, 0)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			SELECT id::text, servicio_id::text, tipo::text, condicion,
			       monto::text, prioridad
			FROM negocio.tarifa
			WHERE ($1::uuid IS NULL OR servicio_id = $1::uuid)
			ORDER BY servicio_id, prioridad DESC`, servicio)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			tarifa, err := escanearTarifa(filas)
			if err != nil {
				return err
			}
			tarifas = append(tarifas, tarifa)
		}
		return filas.Err()
	})
	if err != nil {
		return nil, err
	}

	return tarifas, nil
}

// CrearTarifa publica una tarifa (RF-31).
func (s *Servicio) CrearTarifa(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, nueva api.NuevaTarifa,
) (api.Tarifa, error) {
	// El jsonb viaja como CADENA y no como []byte. pgx manda un []byte en
	// binario, así que PostgreSQL lo recibe como bytea y el cast a jsonb falla
	// con 22P02: un error de sintaxis sobre un JSON que era perfectamente
	// válido. Es un fallo de transporte disfrazado de fallo de contenido.
	crudo, err := json.Marshal(nueva.Condicion)
	if err != nil {
		return api.Tarifa{}, fmt.Errorf("la condición de la tarifa no es un objeto válido: %w", err)
	}
	condicion := string(crudo)

	prioridad := 0
	if nueva.Prioridad != nil {
		prioridad = *nueva.Prioridad
	}

	var tarifa api.Tarifa

	_, err = s.enTx(ctx, tenant, actor, "crear_tarifa", auditoria.RecursoTarifa,
		func(tx pgx.Tx) (string, error) {
			var err error
			tarifa, err = escanearTarifa(tx.QueryRow(ctx, `
				INSERT INTO negocio.tarifa
					(tenant_id, servicio_id, tipo, condicion, monto, prioridad)
				VALUES ($1, $2, $3::negocio.tipo_tarifa, $4::jsonb, $5::numeric, $6)
				RETURNING id::text, servicio_id::text, tipo::text, condicion,
				          monto::text, prioridad`,
				tenant, nueva.ServicioId, string(nueva.Tipo), condicion, nueva.Monto, prioridad))
			if err != nil {
				return "", err
			}
			return tarifa.Id.String(), nil
		})
	if err != nil {
		return api.Tarifa{}, err
	}

	return tarifa, nil
}

// EliminarTarifa la retira (RF-31).
//
// Aquí sí es un DELETE y no un estado, al revés que con los vouchers, y la
// diferencia es que nada la referencia: una tarifa no deja rastro en ninguna
// reserva, porque lo que la reserva guarda es el precio ya calculado.
func (s *Servicio) EliminarTarifa(
	ctx context.Context, tenant uuid.UUID, actor auditoria.Actor, id uuid.UUID,
) error {
	_, err := s.enTx(ctx, tenant, actor, "eliminar_tarifa", auditoria.RecursoTarifa,
		func(tx pgx.Tx) (string, error) {
			return id.String(), borrarPorID(ctx, tx, "negocio.tarifa", id)
		})
	return err
}

// ------------------------------------------------------------ escaneres --

func escanearVoucher(fila pgx.Row) (api.Voucher, error) {
	var (
		voucher api.Voucher
		id      string
		estado  string
		caduca  *time.Time
	)

	if err := fila.Scan(&id, &voucher.Codigo, &voucher.Porcentaje,
		&voucher.LimiteUsos, &voucher.UsosActuales, &caduca, &estado,
		&voucher.CreadoEn); err != nil {
		return api.Voucher{}, err
	}

	var err error
	if voucher.Id, err = uuid.Parse(id); err != nil {
		return api.Voucher{}, err
	}
	voucher.Estado = api.VoucherEstado(estado)
	voucher.CaducaEn = caduca

	return voucher, nil
}

func escanearTarifa(fila pgx.Row) (api.Tarifa, error) {
	var (
		tarifa         api.Tarifa
		id, servicioID string
		tipo           string
		condicion      []byte
	)

	if err := fila.Scan(&id, &servicioID, &tipo, &condicion,
		&tarifa.Monto, &tarifa.Prioridad); err != nil {
		return api.Tarifa{}, err
	}

	var err error
	if tarifa.Id, err = uuid.Parse(id); err != nil {
		return api.Tarifa{}, err
	}
	if tarifa.ServicioId, err = uuid.Parse(servicioID); err != nil {
		return api.Tarifa{}, err
	}
	tarifa.Tipo = api.TipoTarifa(tipo)

	if err := json.Unmarshal(condicion, &tarifa.Condicion); err != nil {
		return api.Tarifa{}, fmt.Errorf("la condición guardada no es un objeto: %w", err)
	}

	return tarifa, nil
}
