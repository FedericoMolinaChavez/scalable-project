// Package datos es la única puerta a PostgreSQL.
//
// Dos restricciones del diseño obligan a que esta capa exista y a que nadie la
// esquive:
//
// Contexto de tenant. Según db/README.md, toda transacción que toque negocio.*
// debe ejecutar set_config('app.tenant_id', $1, true) antes de nada. El true
// es SET LOCAL: PgBouncer corre en modo transacción (ARQ-01), la conexión
// vuelve al pool en cada COMMIT y la toma otro pod, así que un SET de sesión
// filtraría el tenant al siguiente. Por eso el acceso se expone como una
// función que recibe una transacción ya preparada, y no como un pool desnudo.
//
// Modo de ejecución de pgx. Contra PgBouncer en modo transacción no se pueden
// usar prepared statements cacheados por conexión. La configuración del pool
// lo desactiva explícitamente.
//
// Aquí vive también la traducción de errores del motor a errores de dominio:
// 23P01 (exclusion_violation) sobre negocio.reserva significa que el horario ya
// estaba tomado. La invariante la hace cumplir PostgreSQL, no Go.
package datos
