// Package trabajadores es el componente "Trabajadores" de ARQ-01: los bucles
// que hacen lo que no cabe en una petición síncrona.
//
// Todos comparten la misma forma —despertar cada cierto tiempo, hacer una pasada
// acotada, volver a dormir— y las mismas dos reglas, que no son negociables:
//
//	Idempotentes. Una pasada puede repetirse: el proceso muere a mitad, dos
//	réplicas se solapan, el relay publica y no llega a marcar. Nada de lo que
//	hacen puede depender de ejecutarse exactamente una vez.
//
//	Acotados. Cada pasada procesa como mucho un lote. Un barrido sin límite
//	sobre una tabla con meses de historia mantiene una transacción abierta
//	durante minutos, y en PostgreSQL una transacción larga bloquea el vacuum de
//	todo lo demás.
package trabajadores

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// Bucle es un trabajador: un nombre, cada cuánto despierta, y qué hace.
type Bucle struct {
	Nombre    string
	Intervalo time.Duration

	// Pasada devuelve cuántas unidades procesó. El número sirve para decidir si
	// vale la pena volver enseguida: una pasada que llenó el lote es señal de
	// que queda trabajo, y esperar el intervalo entero dejaría acumularse una
	// cola que se puede drenar ya.
	Pasada func(context.Context) (int, error)
}

// Correr arranca todos los bucles y bloquea hasta que se cancele el contexto.
func Correr(ctx context.Context, registro *slog.Logger, bucles ...Bucle) {
	hecho := make(chan struct{}, len(bucles))

	for _, bucle := range bucles {
		go func(b Bucle) {
			defer func() { hecho <- struct{}{} }()
			correrBucle(ctx, registro.With(slog.String("trabajador", b.Nombre)), b)
		}(bucle)
	}

	for range bucles {
		<-hecho
	}
}

func correrBucle(ctx context.Context, registro *slog.Logger, b Bucle) {
	registro.Info("trabajador en marcha", slog.Duration("intervalo", b.Intervalo))

	for {
		procesadas, err := b.Pasada(ctx)

		switch {
		case ctx.Err() != nil:
			// El contexto se canceló durante la pasada: es un apagado, no un
			// fallo, y no merece una línea de error.
			registro.Info("trabajador detenido")
			return

		case err != nil:
			// Un fallo NO detiene el bucle. Un trabajador que muere ante el
			// primer error deja de hacer su trabajo para siempre, y lo que
			// falla suele ser transitorio: la base reiniciándose, un
			// interbloqueo, la red. Se registra y se vuelve a intentar.
			registro.Error("la pasada falló", slog.String("error", err.Error()))

		case procesadas > 0:
			registro.Info("pasada completada", slog.Int("procesadas", procesadas))
		}

		// Si la pasada llenó su lote, se vuelve enseguida: queda cola. Esperar
		// el intervalo entero con trabajo pendiente solo alarga el retraso.
		espera := b.Intervalo
		if err == nil && procesadas >= LoteMaximo {
			espera = 0
		}

		select {
		case <-ctx.Done():
			registro.Info("trabajador detenido")
			return
		case <-time.After(espera):
		}
	}
}

// LoteMaximo acota cuánto procesa una pasada.
//
// Cien filas por transacción: suficiente para drenar rápido, corto para que la
// transacción dure milisegundos. Una transacción larga en PostgreSQL retiene el
// horizonte del vacuum y hace crecer la tabla de todos los demás.
const LoteMaximo = 100

// porCadaTenant ejecuta fn con el contexto de cada tenant activo.
//
// Los trabajadores son globales y RLS es por tenant, así que hay que recorrer:
// una transacción por tenant, cada una con su contexto fijado. La alternativa
// —un rol con BYPASSRLS— haría una sola consulta, pero pondría el aislamiento
// de RNF-06 en manos de que el código del trabajador se acuerde de filtrar, que
// es exactamente lo que RLS existe para no tener que confiar.
//
// El coste es O(tenants) transacciones por pasada. Con cientos de tenants no se
// nota; el día que se note, la respuesta es un rol de trabajador con BYPASSRLS
// y un comentario muy grande explicando por qué, no filtrar a mano por
// costumbre.
func porCadaTenant(
	ctx context.Context, bd *datos.BD, fn func(context.Context, pgx.Tx, string) (int, error),
) (int, error) {
	var tenants []string

	// La lista sale de plataforma.tenant, que no lleva RLS: es el catálogo de
	// negocios, no datos de ninguno.
	if err := bd.SinTenant(ctx, func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx,
			"SELECT id::text FROM plataforma.tenant WHERE estado = 'activo'")
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var id string
			if err := filas.Scan(&id); err != nil {
				return err
			}
			tenants = append(tenants, id)
		}
		return filas.Err()
	}); err != nil {
		return 0, err
	}

	total := 0
	for _, tenant := range tenants {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}

		// Cada tenant en su propia transacción. Una sola para todos haría que
		// un fallo en el último revirtiera el trabajo de los anteriores, y un
		// tenant con datos raros bloquearía a los demás indefinidamente.
		err := bd.EnTenant(ctx, tenant, func(tx pgx.Tx) error {
			n, err := fn(ctx, tx, tenant)
			total += n
			return err
		})
		if err != nil {
			return total, err
		}
	}

	return total, nil
}
