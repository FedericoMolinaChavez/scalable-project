# Backend

Un módulo Go, un binario por componente síncrono de ARQ-01.

```bash
task back:run -- consulta       # :8081
task back:run -- nucleo         # :8080
task back:run -- identidad      # :8082
task back:run -- pagos          # :8084
task back:run -- trabajadores   # :8083
task back:test                  # necesita `task infra:up`
task back:lint
```

Hace falta `TOKEN_SECRETO` en el `.env` (ver `.env.example`): los tres binarios
se niegan a arrancar sin él, porque un servicio que no comprueba la firma de un
token no rechaza nada, sirve los datos igual.

## Qué sirve cada binario

| Binario | Puerto | Rutas | Por qué ahí |
|---|---|---|---|
| `nucleo` | 8080 | `POST /v1/reservas`, `POST /v1/reservas/{id}/cancelacion` | Único que escribe en `negocio.reserva` por la ruta síncrona |
| `consulta` | 8081 | `GET /v1/sedes`, `/v1/servicios`, `/v1/disponibilidad`, `/v1/reservas`, `/v1/reservas/{id}` | Solo lee; en despliegue va contra las réplicas |
| `identidad` | 8082 | `POST /v1/sesiones/codigo`, `/v1/sesiones/token` | Manda correo: es un dominio de fallo propio, y un relé caído no puede arrastrar la ruta de reserva |
| `pagos` | 8084 | `POST /v1/pagos/intencion`, `GET /v1/pagos/{id}/estado`, `POST /v1/webhooks/stripe` | ARQ-01 lo dibuja aparte porque su disponibilidad la acota Stripe: vive fuera del presupuesto de RNF-04, y por eso la reserva se confirma ahí y no en la ruta síncrona |
| `trabajadores` | 8083 | ninguna de negocio (solo sondas y `/metrics`) | El paquete "Asíncrono" de ARQ-01: los ocho bucles |

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
  almacen/              ARQ-01: MinIO. Su único inquilino son los comprobantes
  pagos/                ARQ-01: Webhook de Pagos, en sus dos direcciones
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

**El código de un solo uso no se guarda, se guarda su huella** (RNF-09). SHA-256
a secas, sin sal ni derivación lenta, y eso es correcto porque no es una
contraseña: seis dígitos que viven cinco minutos y aguantan tres intentos se
defienden por el tiempo y por el contador, no por la lentitud del hash. Lo que
compra el hash es que quien lea la tabla no pueda usar lo que ve.

**El contador de intentos se confirma aunque el intento falle.** Es la trampa de
esta parte: devolver el error desde dentro de la transacción revierte, junto con
el error, el incremento que se acababa de escribir, y entonces el límite de
RF-12 A2 no existe. La transacción confirma siempre y el veredicto viaja aparte.
Hay una prueba que falla si alguien lo deshace.

**Un token de invitado no tiene sesión.** `sesion.cuenta_id` es NOT NULL y RF-25
gestiona las sesiones *de una cuenta*; quien reservó como invitado no tiene ni
una cosa ni la otra. Su token es corto, firmado y no revocable, y por eso caduca
pronto: es su única defensa. El formato es propio y no JWT porque el algoritmo
no se negocia —la familia de vulnerabilidades de `alg` no existe si no hay nada
que elegir— y porque los siete componentes viven en un solo módulo Go. Eso deja
de valer en cuanto el Gateway tenga que validar tokens: ahí toca firma
asimétrica estándar.

**Valkey hace exactamente los dos trabajos que le da ARQ-01, y ninguno más.**
Cachea las proyecciones de disponibilidad con los 2 s que RNF-10 autoriza —el
90% del tráfico de RNF-03, que es lo que hace sostenible ese número contra las
réplicas— y sostiene las cuotas de RNF-08. **Nunca es autoridad sobre el cupo**:
el núcleo no lo lee jamás, decide siempre contra PostgreSQL.

De ahí sale una asimetría deliberada en cómo fallan. El caché falla ABIERTO y no
entra en `/listo`: sin él la disponibilidad responde igual, solo que bajando a
las réplicas. El límite de envío de códigos falla CERRADO, porque detrás de él no
hay ninguna otra capa —no existe invariante en el motor que impida mandar
correo— y el coste de negar es que alguien espere, mientras que el de permitir es
correo ilimitado hacia la dirección que alguien escriba.

**El outbox evita la doble escritura.** El núcleo escribe el evento en la MISMA
transacción que la reserva y no publica nada; un relay lo lleva después a
JetStream. Publicar tras el `COMMIT` puede fallar y perder el evento; publicar
antes deja eventos de reservas que se revirtieron. La consecuencia es que la
entrega es **al-menos-una-vez** —la deduplicación la hace NATS con `Nats-Msg-Id`,
que es el `id` del evento— y todo consumidor tiene que ser idempotente.

**`FOR UPDATE SKIP LOCKED` en todos los barridos.** Sin él, dos réplicas del
mismo trabajador seleccionan las mismas filas, la segunda espera a la primera, y
añadir réplicas no acelera nada: solo consume conexiones. Con él se reparten la
cola sin coordinarse, que es la misma filosofía que el resto del sistema.

**El sistema no marca la llegada de nadie.** RF-28 asigna `confirmada → en_curso`
al administrador (el check-in de RF-32), así que las transiciones automáticas son
solo dos: `en_curso → completada` al pasar la hora, y `confirmada → no_show` al
superar el umbral. Una cita que nadie registró acaba en `no_show`, que es lo que
de verdad pasó, no en `completada`.

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

- **Sin cuentas (RF-12).** El tenant llega en la cabecera `X-Tenant-Id` y toda
  reserva es de invitado (`cuenta_id` nulo). Lo que sí existe es la mitad de
  RF-02 que no necesita cuenta: el código de un solo uso al correo con el que se
  reservó. Cuando existan las cuentas hay que poblar `cuenta_id` al crear,
  escribir `plataforma.indice_reserva_global`, y hacer que `GET /v1/reservas`
  acote por cuenta y no por correo de contacto.
- **El alcance de `GET /v1/reservas` sale de quién pide** (RF-23). Hoy solo
  existe el token de invitado, así que devuelve las reservas hechas con el
  correo que ese token acredita. El caso del `administrador` que ve su tenant
  entero (RF-32) llega con RF-12: es un campo más en `consulta.Alcance`, no una
  ruta nueva.
- **RF-32 pide además filtros que el contrato no tiene.** La agenda del
  administrador se recorre por sede y por recurso, y `GET /v1/reservas` solo
  filtra por estado y rango (RF-09). Faltan `sede_id` y `recurso_id`.
- **RF-11 tiene su rollup pero no su superficie.** El trabajador mantiene
  `negocio.metrica_diaria` al día, y no hay ruta que lo sirva: el panel de
  métricas es del administrador, y sin RF-12 no hay forma de saber que quien
  pregunta lo es. Exponerlo bajo `X-Tenant-Id` publicaría los ingresos de cada
  negocio a quien escribiera su identificador.
- **La lista de espera (RF-37) corre y no encuentra nada.**
  `negocio.lista_espera.cuenta_id` es NOT NULL y referencia
  `plataforma.cuenta`, así que hoy nadie puede anotarse. El bucle está montado
  porque su lógica —qué es un cupo liberado, a quién le toca— no depende de cómo
  se autentique quien espera.
- **Solo correo, no SMS.** El canal es un enum en el modelo y RF-02 admite los
  dos; no hay proveedor de SMS, y Mailpit sí está provisionado. Añadir SMS es
  una rama más en el envío.
- **RF-10 sigue en su versión mínima**: el notificador manda el correo de
  creada, confirmada, cancelada, expirada y cupo libre, pero sin plantillas, sin
  `preferencia_notificacion` (RF-21), sin `config_notificacion` (RF-16) y sin
  registro de lo enviado. Hoy no se puede responder «¿se le avisó?» mirando la
  base, solo mirando el buzón.
- **El comprobante es HTML, no PDF** (RF-34). Un PDF exige una biblioteca de
  composición o un navegador headless, y ninguna aporta nada a lo que el
  requisito pide: un documento que se pueda guardar e imprimir. El HTML lleva su
  hoja de estilos de impresión dentro. El día que haga falta firma electrónica,
  eso cambia en `trabajadores/comprobantes.go` y en ningún otro sitio.
- **Sin trazas.** Faltan las dos mitades: el SDK de OpenTelemetry y un colector
  en el compose, donde hoy no hay nada que reciba OTLP.
- **Sin vouchers.** `voucher_codigo` se rechaza con un `422` explícito en vez de
  ignorarse: aceptar un código y cobrar el precio completo sin decirlo es peor
  que negarse.
- **Sin límites de tasa (RNF-08).** El contrato declara el `429`; nada lo emite.
