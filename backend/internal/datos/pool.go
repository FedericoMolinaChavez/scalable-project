package datos

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BD envuelve el pool de conexiones. No expone el *pgxpool.Pool a propósito:
// el acceso a negocio.* tiene que pasar por EnTenant, y un pool público
// invitaría a saltárselo.
type BD struct {
	pool *pgxpool.Pool
}

// Abrir crea el pool ya configurado para PgBouncer en modo transacción.
func Abrir(ctx context.Context, url string) (*BD, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("URL de base de datos inválida: %w", err)
	}

	// ---------------------------------------------------------------------
	// La línea que hace que esto funcione contra PgBouncer.
	//
	// El modo por omisión de pgx es QueryExecModeCacheStatement: prepara cada
	// consulta con un nombre y la reutiliza en la misma conexión. Contra
	// PgBouncer en modo transacción eso se rompe, porque la conexión de
	// servidor vuelve al pool en cada COMMIT y la siguiente transacción puede
	// caer en otra que no tiene ese statement preparado. El síntoma es
	// "prepared statement \"lrupsc_1_0\" does not exist", intermitente y bajo
	// carga, que es la peor forma de descubrirlo.
	//
	// QueryExecModeExec usa el protocolo extendido con statements sin nombre:
	// no deja estado en la conexión, así que sobrevive al multiplexado, y a
	// diferencia del protocolo simple conserva el formato binario y el tipado
	// de parámetros.
	// ---------------------------------------------------------------------
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeExec

	// Por la misma razón, sin cachés que asuman una conexión estable.
	cfg.ConnConfig.StatementCacheCapacity = 0
	cfg.ConnConfig.DescriptionCacheCapacity = 0

	// El pool de la aplicación es pequeño a propósito: quien multiplexa es
	// PgBouncer (default_pool_size), no cada pod. Con 45 pods abriendo pools
	// grandes se agotan las conexiones de PostgreSQL mucho antes de llegar a
	// la carga de RNF-03, que es justo lo que el pooler viene a evitar.
	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("no se pudo crear el pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("no se pudo contactar la base de datos: %w", err)
	}

	return &BD{pool: pool}, nil
}

// Cerrar libera el pool.
func (b *BD) Cerrar() {
	b.pool.Close()
}

// Comprobar es la verificación para la sonda de disponibilidad.
func (b *BD) Comprobar(ctx context.Context) error {
	if err := b.pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}
