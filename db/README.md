# Modelo físico

El DDL que implementa `docs/uml/modelo-datos/`. Los diagramas dicen qué entidades hay
y cómo se relacionan; estos archivos dicen cómo el motor las hace cumplir.

## Cómo aplicarlo

Archivos SQL numerados, aplicados en orden, sin herramienta de migración todavía:

```bash
for f in db/migrations/*.sql; do psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f "$f"; done
```

Cuando haya que aplicarlas desde la aplicación, la recomendación es **goose**:
maneja migraciones en SQL y en Go, y no arrastra el estado *dirty* de
golang-migrate, que en un despliegue con varios pods arrancando a la vez es un
problema real. Añadir las anotaciones `-- +goose Up` a estos archivos es
mecánico; se dejó fuera para que el SQL se lea sin ruido.

No hay migraciones `down`. En una base con datos reales el rollback de un
esquema casi nunca es el inverso de la subida, y escribirlo da una confianza
que no existe. La estrategia es hacia adelante: cada cambio debe ser compatible
con la versión anterior del código el tiempo que dure el despliegue.

## Los tres tenants de la semilla

`semillas/dev.sql` siembra tres, y cada uno tiene un trabajo distinto:

| Identificador | Para qué |
|---|---|
| `estudio-demo` | El negocio de demostración: es lo que enseñan las pantallas |
| `otro-negocio` | El de al lado, para demostrar que RLS lo esconde |
| `pruebas` | Donde escriben las pruebas automatizadas de Go |

El tercero existe por una razón que no es de gusto. `transicion_estado` es
append-only por trigger (RF-28), así que una reserva creada por el núcleo **no
se puede borrar** después sin desactivar la garantía que la propia prueba quiere
que siga en pie: lo más que puede hacer la limpieza es cancelarla. Cada pasada
de `task ci` deja por tanto filas para siempre, y acumuladas en el tenant de
demostración acababan siendo lo que enseñaban las pantallas. En el suyo no le
estorban a nadie.

Es idéntico en forma al de demostración —misma zona, misma moneda, mismo
horario, mismo servicio de una hora a 80.000— para que las pruebas puedan seguir
afirmando "ocho franjas" sin depender de datos que alguien pueda tocar.

## Verificación

```bash
./db/pruebas/ejecutar.sh
```

Levanta un PostgreSQL 17 desechable en Docker, aplica las siete migraciones,
siembra los tenants y corre dos cosas:

- **`pruebas/invariantes.sql`** — 22 casos que comprueban que el *motor* impide
  algo, no que la aplicación se acuerde de impedirlo: solapamiento, citas
  contiguas, cupo liberado al cancelar, referencia cruzada entre tenants, RLS
  en lectura y en escritura, lectura directa de una partición, inmutabilidad de
  políticas, reglas de vouchers, idempotencia de creación. Corre como `app_dev`
  —no como superusuario— para que RLS y los privilegios cuenten de verdad.
- **`pruebas/reserva_concurrente.sql`** — 100 clientes de `pgbench` peleando por
  el mismo cupo. El resultado esperado es exactamente una fila, en todas las
  ejecuciones.

Última ejecución contra PostgreSQL 17.10: las siete migraciones aplican
limpias, las 24 invariantes pasan, y de **1.000 intentos concurrentes sobre el
mismo cupo sobrevivió exactamente 1** (100 clientes × 10 transacciones, 8 hilos,
~7.500 tps, 13 ms de latencia media).

## Contrato con la aplicación

Toda transacción fija su tenant antes de tocar `negocio.*`:

```sql
BEGIN;
SELECT set_config('app.tenant_id', $1, true);   -- true = SET LOCAL
-- ...
COMMIT;
```

El `true` no es opcional. PgBouncer corre en modo transacción (ARQ-01): la
conexión de servidor vuelve al pool en cada `COMMIT` y la toma otro pod. Un
`SET` de sesión sobreviviría a ese cambio y el siguiente tenant heredaría el
contexto del anterior. `set_config(..., true)` se revierte con la transacción.

Sin contexto, `infra.tenant_actual()` devuelve `NULL`, ninguna política de RLS
se satisface y las consultas no devuelven filas. Falla cerrado.

## Orden de las migraciones

| Archivo | Contenido |
|---|---|
| `0001_infraestructura.sql` | Esquemas, `btree_gist`, roles, contexto de tenant, particionado, inmutabilidad |
| `0002_tipos.sql` | Enumerados |
| `0003_plataforma_identidad.sql` | `tenant`, `cuenta`, `indice_reserva_global` |
| `0004_negocio_catalogo.sql` | `sede`, `servicio`, `recurso`, `servicio_recurso`, `regla_disponibilidad`, `excepcion_calendario` |
| `0005_negocio_politica_voucher.sql` | `politica_version`, `voucher` — adelantadas de ER-03 porque `reserva` las referencia |
| `0006_negocio_reserva.sql` | `reserva`, restricción de exclusión, `uso_voucher`, `transicion_estado`, `lista_espera`, `calificacion` |
| `0007_rls_y_privilegios.sql` | RLS por tenant y reparto de privilegios |

## Las cuatro decisiones que hay que entender antes de tocar nada

**La invariante vive en el motor.** `EXCLUDE USING gist (tenant_id WITH =,
recurso_id WITH =, periodo WITH &&) WHERE estado IN ('pendiente','confirmada')`.
Es lo único que sigue siendo cierto con 45 pods escribiendo sin coordinarse.
Se aplica partición por partición, y eso basta: un recurso pertenece a un solo
tenant, así que todas sus reservas caen siempre en la misma partición.

**Los períodos son `[)`.** Límite inferior cerrado, superior abierto. Con
límites cerrados por ambos lados, 10:00–11:00 y 11:00–12:00 comparten un
instante y `&&` las declara solapadas: el motor rechazaría dos citas
consecutivas válidas. La aplicación debe construir siempre
`tstzrange(inicio, fin, '[)')`.

**El predicado del índice no puede mirar el reloj.** No dice
`pendiente AND expira_en > now()` porque PostgreSQL exige predicados inmutables.
La consecuencia es de arquitectura, no de SQL: un bloqueo vencido sigue
ocupando el cupo hasta que alguien lo marque `expirada`. Por eso existe el
trabajador expirador de RF-27, y su frecuencia determina cuánto tiempo un cupo
libre parece ocupado.

**Las claves foráneas arrastran `tenant_id`.** Todas son compuestas:
`(tenant_id, x_id) → (tenant_id, id)`. Una referencia entre tenants distintos
no es un error a evitar, es una fila que el motor rechaza.

## Particionado

64 particiones `HASH (tenant_id)` en toda tabla de `negocio`. Potencia de dos a
propósito: PostgreSQL admite módulos mixtos, así que una partición se puede
`DETACH` y reemplazar por dos con el módulo duplicado. Pasar de 64 a 100 no es
posible sin reescribir.

Límite conocido: HASH reparte tenants, no carga. Un tenant muy grande convierte
su partición en un punto caliente. La salida sería sacarlo a su propia
partición por LIST, no subir el número de particiones.

## Roles

| Rol | Uso | RLS |
|---|---|---|
| `reservas_app` | Núcleo de escritura y configuración | Sujeto |
| `reservas_lectura` | Consulta y disponibilidad, contra réplicas | Sujeto, solo `SELECT` |
| `reservas_soporte` | `super_admin` (RF-23) y purga de tenants | `BYPASSRLS` |

Son roles de grupo `NOLOGIN`. Los usuarios de conexión reales los crea
CloudNativePG con credenciales en sealed secrets. Ninguna contraseña entra a
una migración versionada.

Los roles de aplicación tienen privilegios sobre las tablas **padre** y sobre
ninguna partición. No es un detalle: consultar `negocio.reserva_p07`
directamente usaría las políticas de esa partición —que no tiene ninguna— y
devolvería filas de todos los tenants del bucket. Acceder por el padre no
requiere privilegios sobre los hijos, así que revocarlos no cuesta nada.

## Estado de ER-02 y ER-03

ER-02 está completo. Las cinco tablas que faltaban entraron en `0012`
—`sesion`, `preferencia_notificacion`, `agente`, `autorizacion_agente` y
`token_agente`— junto con dos correcciones sobre lo que ya existía:

- `cuenta_contacto_minimo` prohibía la anonimización que RF-25 exige y que un
  comentario de `0003` ya daba por hecha: la tabla documentaba un borrado que
  ella misma impedía. Ahora exceptúa las cuentas eliminadas.
- No había índice para buscar un token por su valor, que es como se canjea un
  ENLACE. Los dos de `0008` entran por destino, que es lo que necesita un
  código —quien lo teclea escribe también su correo— y no lo que necesita un
  enlace, que viaja solo en la URL.

El aislamiento de `plataforma.*` sigue siendo por CUENTA y no por tenant, y
sigue sostenido por la capa de autorización: una política de RLS
`tenant_id = infra.tenant_actual()` sobre estas tablas rompería justo su razón
de existir, que es que una persona sea la misma aunque reserve en veinte
negocios.

## Pendiente

- Resto de ER-03 (dinero): `tarifa`, `pago`, `evento_webhook`, `reembolso`,
  `comprobante`.
- Operación: `config_notificacion`, `notificacion_programada`,
  `evento_auditoria` (`PARTITION BY RANGE`) y `metrica_diaria`. `outbox_evento`
  ya existe desde `0010`.
- Decidir si `clave_idempotencia` en `reserva` se queda. Es un añadido respecto
  a ER-01: el modelo tenía idempotencia en el webhook, el reembolso y la
  notificación, pero no en la creación, y un agente que reintenta un `POST`
  tras un timeout crearía dos bloqueos que nadie libera hasta el TTL.
