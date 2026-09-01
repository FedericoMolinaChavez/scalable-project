package datos

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EnTenant ejecuta fn dentro de una transacción con el contexto de tenant ya
// fijado. Es la única forma de tocar negocio.* desde el código.
//
// El contrato lo define db/README.md: toda transacción fija su tenant antes de
// nada con set_config('app.tenant_id', $1, true). El tercer parámetro en true
// significa SET LOCAL, y no es opcional. PgBouncer corre en modo transacción
// (ARQ-01), así que la conexión de servidor vuelve al pool en cada COMMIT y la
// toma otro pod; un SET de sesión sobreviviría a ese cambio y el siguiente
// tenant heredaría el contexto del anterior. Con SET LOCAL se revierte junto
// con la transacción.
//
// Sin contexto, infra.tenant_actual() devuelve NULL, ninguna política de RLS
// se satisface y las consultas no devuelven filas. Falla cerrado: el peor caso
// es no ver datos, nunca ver los de otro tenant.
//
// Si fn devuelve error se revierte; si no, se confirma. Los errores del motor
// salen ya traducidos a los de este paquete.
func (b *BD) EnTenant(ctx context.Context, tenantID string, fn func(pgx.Tx) error) error {
	if tenantID == "" {
		return ErrSinTenant
	}

	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo abrir la transacción: %w", traducir(err))
	}

	// Rollback tras un Commit correcto devuelve ErrTxClosed y es inofensivo:
	// garantiza que ningún camino de salida deje la transacción abierta.
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID); err != nil {
		return fmt.Errorf("no se pudo fijar el tenant: %w", traducir(err))
	}

	if err := fn(tx); err != nil {
		return traducir(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("no se pudo confirmar: %w", traducir(err))
	}

	return nil
}

// SinTenant ejecuta fn contra plataforma.* e infra.*, que no están sujetos a
// RLS por tenant: el catálogo de tenants, la identidad y las tablas de
// infraestructura.
//
// Existe como función aparte y con nombre incómodo a propósito. Si el acceso
// sin tenant fuera el camino cómodo, acabaría usándose para negocio.* y el
// aislamiento se perdería sin que nadie lo note.
func (b *BD) SinTenant(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("no se pudo abrir la transacción: %w", traducir(err))
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return traducir(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("no se pudo confirmar: %w", traducir(err))
	}

	return nil
}

// ErrSinTenant se devuelve cuando se intenta abrir una transacción de negocio
// sin identificar el tenant.
var ErrSinTenant = errors.New("falta el identificador de tenant")
