# Backend

Un módulo Go, un binario por componente síncrono de ARQ-01.

```bash
task back:run -- consulta   # :8081
task back:run -- nucleo     # :8080
task back:test              # necesita `task infra:up`
task back:lint
```

## Qué sirve cada binario

| Binario | Puerto | Rutas | Por qué ahí |
|---|---|---|---|
| `nucleo` | 8080 | `POST /v1/reservas` | Único que escribe en `negocio.reserva` por la ruta síncrona |
| `consulta` | 8081 | `GET /v1/sedes`, `/v1/servicios`, `/v1/disponibilidad`, `/v1/reservas`, `/v1/reservas/{id}` | Solo lee; en despliegue va contra las réplicas |

**`/v1/reservas` lo sirven los dos**, y no es un descuido: el `POST` tiene que
ser atómico con la invariante y el `GET` no, que es exactamente la frontera por
la que ARQ-01 descompone. En desarrollo eso obliga a que el proxy de Vite
enrute por método; en producción lo hace el Gateway.

El código de catálogo y disponibilidad ya vive en sus propios paquetes
(`internal/catalogo`, `internal/disponibilidad`), que en ARQ-01 son componentes
aparte. Los monta `consulta` porque todavía no tienen despliegue propio;
separarlos será mover dos líneas de `cmd/consulta/main.go`.

## Paquetes

```
cmd/                    un main por binario, y nada más
internal/
  api/                  GENERADO desde api/openapi.yaml — no editar
  plataforma/           arranque común: config, registro, métricas, sondas, apagado
  datos/                única puerta a PostgreSQL; contexto de tenant y traducción de errores
  dominio/              las reglas que el motor NO hace cumplir
  transporte/           middleware y el formato de error RFC 9457
  rutas/                monta el HTTP sobre los componentes y traduce errores a códigos
  catalogo/             ARQ-01: Configuración y Catálogo
  disponibilidad/       ARQ-01: Servicio de Disponibilidad
  nucleo/               ARQ-01: Núcleo de Reservas (escritura)
  consulta/             ARQ-01: Consulta de Reservas (lectura)
  reservas/             proyección de una fila de reserva, compartida por núcleo y consulta
  pruebas/              apoyo para las pruebas de integración
```

## Decisiones que conviene entender

**La invariante vive en el motor, y el código la traduce, no la reimplementa.**
El núcleo no comprueba disponibilidad y luego inserta —eso sería una carrera con
una ventana entre las dos consultas—: inserta y traduce el rechazo. Un `23P01`
sobre `negocio.reserva` es `ErrHorarioOcupado`, y eso es un `409`, que no es un
fallo sino el resultado normal de perder una carrera por el mismo cupo (RF-01,
alt. 3).

**La disponibilidad se calcula en SQL, no en Go.** Es una resta de conjuntos
—reglas, menos excepciones, menos reservas— y PostgreSQL ya tiene los tipos y
los índices para hacerla, incluido el mismo GiST que sostiene la restricción
EXCLUDE. Traerse las tres tablas a memoria para restarlas aquí significaría
transferirlas enteras en cada una de las peticiones más frecuentes del sistema
(el 90% del tráfico, RNF-03).

**Las pendientes vencidas se reconcilian por los dos lados.** El predicado de la
restricción EXCLUDE no puede excluirlas: PostgreSQL exige que el predicado de un
índice sea inmutable y `now()` no lo es. Así que la lectura las descarta
(`expira_en > now()`) y el núcleo las recicla dentro de su propia transacción
antes de insertar. No sustituye al expirador de RF-27 —que barre la tabla entera
y todavía no existe—, cubre el caso concreto que está a punto de estorbar.

**Los códigos de estado se deciden en un solo sitio.** `internal/rutas/errores.go`
es el único que conoce a la vez los errores del dominio y los códigos HTTP. Con
la traducción repartida por los manejadores, el mismo error acaba siendo un 422
en una ruta y un 500 en otra, y la promesa de que un error sea indistinguible
venga del componente que venga se rompe sin que ninguna prueba lo note.

**Cada operación solo puede responder lo que el contrato declara.** El generador
produce un tipo de respuesta por código y por operación, así que devolver un 404
en una ruta que no lo declara no compila. Es verboso a propósito: el fallo que
evita solo aparecería en ejecución y contra un frontend ya desplegado.

## Pruebas

Corren contra la base real de `deploy/docker-compose.yml`, a través de PgBouncer.
No se sustituye la base por un doble: lo que se comprueba —RLS, la restricción
EXCLUDE, el `SET LOCAL` bajo un pooler que multiplexa conexiones, el cálculo de
disponibilidad en SQL— no existe en un doble.

Sin `DATABASE_URL` se omiten, para que `go test ./...` siga siendo útil sin
infraestructura levantada. En CI la infraestructura está.

**Las pruebas escriben en su propio tenant** (`pruebas`), no en el de
demostración. `transicion_estado` es append-only por trigger (RF-28), así que
una reserva creada por el núcleo no se puede borrar después sin desactivar la
garantía que la prueba quiere que siga en pie: la limpieza solo puede
cancelarla. Cada pasada deja filas para siempre, y en el tenant de demostración
acababan siendo lo que enseñaban las pantallas.

`go test` corre los paquetes en **paralelo** y ese tenant tiene un solo recurso,
así que además cada paquete usa su propio día de la semana (`internal/pruebas`).
Sin ese reparto, dos paquetes reservando el mismo día se estorbarían con 409 y
recuentos cambiantes que no tienen nada que ver con lo que cada uno comprueba.

## Pendiente

- **Sin autenticación (RF-12).** El tenant llega en la cabecera `X-Tenant-Id` y
  toda reserva es de invitado (`cuenta_id` nulo). Al crear, cuando exista el
  token, hay que poblar `cuenta_id` y escribir `plataforma.indice_reserva_global`.
- **`GET /v1/reservas` está sin acotar por rol.** Su alcance no es fijo: sale
  del tipo de cuenta (RF-23). Un `usuario` ve las suyas (RF-02), un
  `administrador` las de su tenant (RF-32). Sin token no hay de dónde derivarlo
  y devuelve las del tenant entero, que es el alcance del administrador. La ruta
  es la correcta —RF-23 modela el alcance como intersección, no como rutas
  distintas por rol—, pero **hoy cualquiera ve lo que solo el administrador
  debería ver**, y eso no puede llegar a producción sin RF-12.
- **RF-32 pide además filtros que el contrato no tiene.** La agenda del
  administrador se recorre por sede y por recurso, y `GET /v1/reservas` solo
  filtra por estado y rango (RF-09). Faltan `sede_id` y `recurso_id`.
- **RF-02 tiene un segundo camino de identificación** que nada implementa: un
  OTP por teléfono o email, para que quien reservó como invitado pueda ver sus
  reservas sin tener cuenta.
- **Sin Stripe (RF-33).** El `201` no trae `pago_client_secret`, y la reserva se
  queda pendiente hasta que venza. La confirmación llega por webhook, que no
  entra en esta rebanada.
- **Sin trabajadores.** No hay expirador (RF-27), ni conciliador de pagos, ni
  notificaciones, ni relay del outbox.
- **Sin vouchers.** `voucher_codigo` se rechaza con un `422` explícito en vez de
  ignorarse: aceptar un código y cobrar el precio completo sin decirlo es peor
  que negarse.
- **Sin límites de tasa (RNF-08).** El contrato declara el `429`; nada lo emite.
