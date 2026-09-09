# Backend

Un módulo Go, un binario por componente síncrono de ARQ-01.

```bash
task back:run -- consulta       # :8081
task back:run -- nucleo         # :8080
task back:run -- identidad      # :8082
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
| `identidad` | 8082 | Todo `/v1/sesiones`, `/v1/cuentas` y `/v1/agentes` | Manda correo: es un dominio de fallo propio, y un relé caído no puede arrastrar la ruta de reserva |
| `trabajadores` | 8083 | ninguna de negocio (solo sondas y `/metrics`) | El paquete "Asíncrono" de ARQ-01: expirador (RF-27), transiciones automáticas (RF-28), relay del outbox y notificador (RF-10) |

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
  dominio/              las reglas que el motor NO hace cumplir, y el alcance de RF-23
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

**Un token de invitado no tiene sesión, y uno de cuenta sí.** `sesion.cuenta_id`
es NOT NULL y RF-25 gestiona las sesiones *de una cuenta*; quien reservó como
invitado no tiene ni una cosa ni la otra. Su token es corto, firmado y no
revocable, y por eso caduca pronto: es su única defensa.

Con cuenta aparecen los dos tokens de RF-12, y la asimetría entre ellos es
deliberada. El de ACCESO va firmado y se verifica sin tocar la base —es lo que
le permite sostener las ~30.000 lecturas/s de RNF-03—, así que revocarlo de
verdad exigiría una consulta por petición. El de REFRESCO se canjea contra
`plataforma.sesion` y ahí sí se comprueba `revocada_en`. La consecuencia hay que
aceptarla con los ojos abiertos: **revocar una sesión corta el refresco al
instante y el acceso al vencer**, de modo que la ventana de revocación es
exactamente `TTL_ACCESO`. Acortarla se hace bajando ese número, no añadiendo una
lectura a la ruta caliente.

El refresco además **rota** en cada canje. Es lo que hace que un refresco
copiado se note —el primero de los dos en canjearlo deja al otro fuera— en vez
de quedar utilizable en paralelo hasta que caduque la sesión.

**El formato es propio y no JWT** porque el algoritmo no se negocia —la familia
de vulnerabilidades de `alg` no existe si no hay nada que elegir— y porque los
siete componentes viven en un solo módulo Go. Eso deja de valer en cuanto el
Gateway tenga que validar tokens: ahí toca firma asimétrica estándar. La versión
del prefijo es `rv2` desde que el token lleva cuenta, tipo y tenant: un token del
formato anterior se rechaza en la primera comparación en vez de interpretarse
con reglas que no eran las suyas.

**Argon2id para contraseñas y SHA-256 para todo lo demás**, y no es una
inconsistencia. Un código de seis dígitos vive cinco minutos y aguanta tres
intentos: su defensa es el tiempo y el contador, y un hash lento no compraría
nada. Un refresco son 256 bits de `crypto/rand`: no hay diccionario que probar.
Una contraseña la elige una persona, vive años y se ataca OFFLINE si alguien se
lleva la tabla, donde no hay contador que valga; ahí lo único que encarece el
ataque es que cada intento cueste memoria. Los parámetros viajan dentro del hash
en formato PHC, así que subirlos más adelante no invalida lo ya guardado.

**El correo solo viaja dentro del token si está VERIFICADO.** De eso cuelga algo
concreto: el alcance de una cuenta incluye las reservas que esa persona hizo
como invitado con ese correo (RF-24). Si bastara con escribirlo en el perfil,
poner la dirección de otra persona sería suficiente para heredar sus reservas.
Cambiar el correo lo deja sin verificar (RF-22), así que la sesión siguiente ya
no lo lleva.

**Un correo que no existe tarda lo mismo en responder que uno que sí.** El
inicio de sesión verifica contra un hash de referencia cuando la cuenta no
aparece. Sin eso, la anti-enumeración de RF-12 A12 se filtra por el reloj: el
"no encontrado" vuelve en microsegundos y el "contraseña incorrecta" tarda los
~50 ms de Argon2id, y esa diferencia es un oráculo tan bueno como responder
mensajes distintos, solo que invisible en el código si nadie lo escribe a
propósito.

**El alcance de RF-23 es un tipo, `dominio.Alcance`, y no un parámetro suelto.**
Lo usan por igual la lectura y la escritura, porque una misma petición no puede
significar una cosa listando y otra cancelando. Los tres casos salen del token:
un `usuario` ve lo suyo (RF-02), un `administrador` ve su tenant entero (RF-32),
un invitado ve lo hecho con su correo, y un agente ve lo de la cuenta que
representa (RF-05). El alcance va en el WHERE y nunca en un `if` posterior:
filtrar después significaría que la base devolvió filas ajenas y que solo un
condicional impidió enseñarlas.

**Un administrador opera sobre su tenant, salga lo que salga en la cabecera.**
El tenant va dentro de su token; aceptar el de `X-Tenant-Id` permitiría leer los
datos de cualquier otro negocio escribiendo su identificador. Para las rutas
públicas —catálogo, disponibilidad, crear reserva— la cabecera sigue mandando,
porque ahí no hay token del que derivar nada.

**Las reservas de invitado se reclaman al LEER, no al verificar.** RF-24 dice
"asocia las reservas previas hechas como invitado con el mismo correo", y una
escritura que las reasignara todas tendría que barrer las 64 particiones de cada
tenant: `negocio.reserva` está particionada por tenant y no hay índice global de
reservas de invitado. La consulta, en cambio, ya está acotada al tenant que se
está mirando, y lo que ve la persona es lo mismo.

**Un agente nunca recibe alcance propio.** RF-13 emite su token como
`acciones pedidas ∩ alcance de la cuenta impersonada`, y esa intersección se
calcula al emitir y viaja firmada dentro del token. Ninguna ruta puede
ampliarla después. Las tres razones de rechazo —credencial que no vale, cuenta
que no lo autorizó (RNF-07), acciones fuera de su alcance— responden el mismo
403: separarlas le diría a quien prueba credenciales qué parte ya superó.

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

- **Solo correo, no SMS.** El canal es un enum en el modelo y RF-02, RF-12 y
  RF-19 admiten los dos; no hay proveedor de SMS y Mailpit sí está
  provisionado. La verificación por teléfono devuelve un `422` explícito en vez
  de aceptar y no enviar nada, que dejaría a alguien esperando un mensaje que no
  existe.
- **Sin 2FA (RF-12 A7).** El diagrama lo contempla como un paso opcional y el
  modelo no tiene dónde guardar el secreto: es una columna más en `cuenta` y una
  rama más en el inicio de sesión.
- **El `usado_en` de `token_agente` no se escribe.** El token de agente se
  verifica solo por firma, como todos los demás, así que nada consulta la fila
  al usarlo. Registrar el primer uso exige una lectura en la ruta del agente, y
  entra con la auditoría de RF-36, que es quien la necesita.
- **`super_admin` no tiene superficie.** Existe en el modelo, en RF-23 y en el
  rol `reservas_soporte`, pero ninguna ruta lo distingue de un administrador.
  Con él llegan RF-35 (alta de tenant) y la consulta global de auditoría.
- **Un administrador no se crea desde aquí.** RF-35 dice que serlo se deriva de
  ser dueño de un tenant, y ese alta es del `super_admin`: hoy la única forma de
  tener uno es escribir la fila.
- **El reembolso de una cancelación no ocurre** (RF-29). Cancelar libera el cupo
  y escribe su transición; el dinero lo mueve un trabajador que no existe.
- **Cuatro de los ocho trabajadores de ARQ-01 siguen sin existir**: conciliador
  de pagos (RF-33), procesador de reembolsos (RF-29), rollup de métricas (RF-11)
  y lista de espera (RF-37). Los cuatro dependen de tablas de ER-03 que aún no
  se han creado o de Stripe.
- **RF-10 está en su versión mínima**: el notificador manda el correo, pero sin
  plantillas, sin `config_notificacion` (RF-16) y sin registro de lo enviado.
  Las preferencias de RF-21 ya se guardan; lo que falta es que el envío las
  consulte.
- **Sin trazas.** Faltan las dos mitades: el SDK de OpenTelemetry y un colector
  en el compose, donde hoy no hay nada que reciba OTLP.
- **MinIO sigue levantado y sin usar.** Su trabajo en ARQ-01 son los
  comprobantes de RF-34, que dependen del pago de RF-33.
- **Sin Stripe (RF-33).** El `201` no trae `pago_client_secret`, y la reserva se
  queda pendiente hasta que venza.
- **Sin vouchers.** `voucher_codigo` se rechaza con un `422` explícito en vez de
  ignorarse: aceptar un código y cobrar el precio completo sin decirlo es peor
  que negarse.
- **Los límites de RNF-08 solo cubren identidad.** Envío de códigos y enlaces,
  intentos de inicio de sesión e intercambios de agente pasan por Valkey; las
  rutas de catálogo, disponibilidad y reserva declaran el `429` y nadie lo
  emite todavía.
