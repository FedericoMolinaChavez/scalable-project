# Infraestructura de desarrollo local

```bash
task infra:up       # levantar
task infra:estado   # ver salud
task infra:psql     # consola SQL
task infra:down     # parar, conservando los datos
task infra:reset    # parar y BORRAR los datos
```

## Qué es y qué no es

Esto **no modela ARQ-03**. No hay multi-AZ, ni replicación por quórum, ni
failover, ni Cilium. Su único trabajo es dar a las fases 3 a 6 una base de
datos con el esquema ya aplicado, para escribir Go y React con `go run` y
`vite dev` en el host.

Los objetivos de RNF-04 —failover, pérdida de una zona— se demuestran aparte,
sobre Kubernetes con CloudNativePG. Son una entrega del trabajo, no un entorno
de desarrollo, y no tienen por qué estar en el camino de cada edición de
código.

## Lo único que sí reproduce a propósito

**PgBouncer en modo transacción, y PostgreSQL inalcanzable sin él.**

El 5432 no se publica al host. No es un descuido: si se pudiera llegar directo
a PostgreSQL, el desarrollo esquivaría el pooler sin darse cuenta y las dos
restricciones que impone el modo transacción solo aparecerían en producción.

| Restricción | Consecuencia en el código |
|---|---|
| La conexión vuelve al pool en cada `COMMIT` | El contexto de tenant va con `set_config(..., true)` — SET LOCAL. Un `SET` de sesión filtraría el tenant al siguiente cliente |
| No hay estado de sesión estable | pgx no puede cachear *prepared statements* |

Está en `backend/internal/datos/doc.go` y en `pgbouncer/pgbouncer.ini`.

## Servicios

| Servicio | Puerto en el host | Para qué |
|---|---|---|
| PostgreSQL 17 | *(ninguno, a propósito)* | Autoridad sobre el cupo |
| PgBouncer | 6432 | Única puerta a la base |
| Valkey | 6379 | Límites de tasa y proyecciones (RNF-08, RNF-01) |
| NATS JetStream | 4222, 8222 | Relay del outbox |
| MinIO | 9000, 9001 | Comprobantes (RF-34) |
| Mailpit | 1025, 8025 | Captura el correo de RF-10 sin enviarlo fuera |

## El esquema

Se aplica solo la primera vez, cuando el volumen está vacío: es como funciona
`docker-entrypoint-initdb.d`. `postgres/aplicar-esquema.sh` corre las siete
migraciones en orden y después `db/semillas/dev.sql`, que es quien crea el rol
de conexión `app_dev`.

Si cambias una migración, el contenedor **no** la reaplica. Hace falta
`task infra:reset`. Es coherente con la estrategia de `db/README.md`: no hay
migraciones `down`, solo hacia adelante.

## Credenciales

Todas de desarrollo, todas en el repositorio a propósito: no protegen nada.
`app_dev` / `dev` lo crea la semilla. En producción no existe este archivo —
los usuarios de conexión los crea CloudNativePG con sealed secrets.
