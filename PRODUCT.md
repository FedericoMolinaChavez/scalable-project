# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Cuatro audiencias confirmadas, sobre un mismo producto multi-tenant. Las cuatro
entran en el alcance de diseño; la primera es la primaria.

**Cliente final (`usuario`) — primaria.** Una persona sin tenant propio que
reserva en los negocios que quiera. Llega para conseguir un horario concreto:
elige sede y servicio, mira la disponibilidad, reserva, paga, y después vuelve a
consultar, cancelar o modificar lo que ya tiene. Es tráfico esporádico y de alta
intención: casi nunca ha usado la interfaz antes y no volverá a verla en semanas.

**Administrador del tenant (`administrador`).** El dueño del negocio, acotado a
UN tenant. Configura lo que hace posible reservar —sedes, servicios, recursos,
reglas de disponibilidad, excepciones de calendario, precios, políticas de
cancelación, vouchers, notificaciones— y revisa las métricas de su operación
(RF-11). Uso recurrente, denso en datos, en escritorio.

**Agente.** Opera en nombre de otra cuenta (RF-04, RF-05, RF-13): reserva y
gestiona por teléfono o presencialmente mientras el cliente espera. Su alcance
efectivo es siempre el subconjunto `acción ∩ alcance de la cuenta impersonada`,
nunca más. La interfaz tiene que dejar visible en todo momento en nombre de
quién se está actuando.

**Cuenta en alta o recuperación.** Registro, verificación de email y teléfono,
inicio de sesión, cambio y recuperación de contraseña, alta de tenant nuevo
(RF-12, RF-18, RF-19, RF-24, RF-35). No es una audiencia distinta, es el estado
inicial de las tres anteriores, y en el caso del alta de tenant es el momento en
que un cliente final se convierte en administrador.

`super_admin` (operador de la plataforma) existe en el modelo de datos, en
RF-23 y en el rol `reservas_soporte`, pero ninguna ruta lo distingue todavía de
un administrador. Si tiene superficie propia sigue siendo una decisión abierta;
lo que sí está claro es qué haría en ella: el alta de tenants de RF-35 y la
consulta global de auditoría de RF-36.

## Product Purpose

Plataforma SaaS de reservas multi-tenant: cualquier negocio que venda tiempo
sobre recursos limitados —una sala, una silla, un profesional, un equipo— se da
de alta como tenant, publica su catálogo y su disponibilidad, y recibe reservas
pagadas sin montar nada propio.

El producto tiene éxito cuando una reserva pagada nunca se convierte en un
conflicto: dos personas no pueden quedarse con el mismo cupo, el cupo que alguien
está pagando no se lo llevan mientras paga, y el que no se paga vuelve al
mercado solo. Todo lo demás —catálogo, notificaciones, métricas, vouchers— existe
para sostener esa transacción.

## Positioning

La invariante de no-solapamiento no vive en la aplicación: vive en el motor. Una
restricción `EXCLUDE USING gist (tenant_id, recurso_id, periodo)` sobre los
estados `pendiente` y `confirmada` hace que el solapamiento sea físicamente
imposible de insertar, y el Núcleo de Reservas la resuelve en UNA transacción
junto con la validación de cuota, el consumo del voucher y la escritura del
outbox. Un competidor puede copiar las pantallas; no puede copiar esa garantía
sin rehacer su frontera transaccional.

De ahí salen dos consecuencias que la interfaz hereda y no puede disimular:

- **El bloqueo temporal ES la reserva.** No hay tabla de bloqueos aparte: una
  reserva `pendiente` con `expira_en` es el bloqueo (RF-27). Reservar y "apartar
  mientras pago" son el mismo acto, con reloj.
- **La disponibilidad es una lectura optimista.** El servicio de disponibilidad
  —90% del tráfico— sirve desde caché con hasta 2 segundos de desactualización
  autorizados por RNF-10. Puede mostrar un cupo que otro acaba de tomar. Quien
  decide es el núcleo al insertar, y el `409` resultante es un desenlace normal
  del flujo (RF-01, alt. 3), no un fallo del sistema.

## Operating Context

**El escenario del cliente final.** Entra por un enlace o una búsqueda, muchas
veces desde el móvil y con prisa. Compara horarios contra su propia agenda, que
está fuera de la pantalla. Elige, y a partir de ahí corre un reloj: tiene que
completar sus datos, aceptar términos, pasar un captcha y pagar antes de que
`expira_en` venza y el cupo se libere. Después del pago, la confirmación no es
inmediata ni síncrona: llega por el webhook de Stripe (RF-33), fuera del
presupuesto de latencia del núcleo.

**El escenario del administrador.** Sesión larga en escritorio, configurando
reglas que se aplicarán a semanas de calendario a la vez. Sus errores son caros y
diferidos: una regla de disponibilidad mal puesta no falla al guardarse, falla
cuando alguien reserva o cuando nadie puede.

**El escenario del agente.** Habla con una persona mientras usa la interfaz. No
puede leer la pantalla en silencio ni volver atrás cómodamente; necesita ver de
quién es la cuenta, qué puede hacer sobre ella y qué acaba de pasar, sin
navegar.

**Zona horaria y moneda son del tenant, no del navegador** (RF-38). Cada sede
lleva además la suya. Un horario mostrado en la zona equivocada es un error de
producto, no de formato.

## Capabilities and Constraints

**Estados de una reserva** (`negocio.estado_reserva`): `pendiente` (es el
bloqueo, lleva `expira_en`), `confirmada` (la promovió el webhook de pago),
`en_curso`, `completada`, `cancelada`, `no_show`, `expirada`. Toda transición
queda registrada en `transicion_estado`, que es inmutable por trigger, con el
tipo de actor que la causó (`usuario`, `administrador`, `agente`, `sistema`,
`super_admin`).

**Vocabulario del dominio, tal como está en el modelo.** Tenant, sede, servicio,
recurso, regla de disponibilidad, excepción de calendario (`feriado`,
`mantenimiento`, `cierre`), política (versionada), voucher, reserva, lista de
espera, calificación. La interfaz usa estos términos; renombrarlos crea dos
vocabularios para lo mismo.

**El período de una reserva es semiabierto `[inicio, fin)`.** El instante final
NO pertenece a la reserva: es justo lo que permite que la siguiente cita empiece
a las 11:00 cuando la anterior termina a las 11:00. Se muestra como "10:00–11:00"
porque es lo natural para una persona, pero ningún cálculo puede tratar `fin`
como ocupado.

**Alcance por tipo de cuenta** (RF-23), aplicado en el motor con RLS y no solo
en la aplicación: `usuario` sobre sus propios datos; `administrador` sobre los
recursos de su tenant; `super_admin` global. Un agente nunca recibe más alcance
del que ya tiene la cuenta impersonada.

**El alcance es de la cuenta, no de la pantalla.** Una misma acción —listar
reservas— devuelve conjuntos distintos según quién la pida: las suyas a un
cliente (RF-02), las del tenant a un administrador (RF-32). No son dos
funciones, es una con el alcance acotado por la cuenta. Aun así son dos
superficies distintas de diseñar: la del cliente es una lista de lo suyo; la del
administrador es una agenda por sede y recurso con acciones sobre cada reserva.

**Un cliente puede consultar sus reservas sin tener cuenta.** RF-02 admite dos
métodos de identificación: iniciar sesión, o recibir un OTP en el teléfono o el
correo con que reservó. Y quien se registra después no pierde lo de antes: una
cuenta ve también las reservas que hizo como invitado con su correo ya
verificado (RF-24). Es la contrapartida de permitir reservar como invitado,
y significa que la pantalla de «mis reservas» tiene dos puertas de entrada, no
una. El envío del OTP responde igual exista o no una cuenta asociada, para no
permitir enumerar usuarios.

**Un `409` no se reintenta.** Ningún 4xx se reintenta en el cliente: significa
que otra transacción se quedó con el cupo, e insistir multiplica la carga sobre
el núcleo justo cuando hay contención. Solo se reintentan los 5xx y los fallos
de red, dos veces.

**La disponibilidad se cachea 2 segundos** en el cliente, exactamente la
desactualización que RNF-10 autoriza.

**Toda pantalla con datos remotos cubre tres estados**: cargando, error y vacío.
Está impuesto por el componente `EstadoConsulta`, no por disciplina.

**Los tipos del frontend se generan desde `api/openapi.yaml`**, no se escriben.
Un cambio de contrato rompe la compilación de los dos lados en vez de aparecer
en producción.

Constraints y decisiones abiertas:

- **La autenticación existe en el backend y no en la interfaz.** RF-12, RF-18,
  RF-19, RF-21, RF-22, RF-24 y RF-25 están implementados y contratados; ninguna
  pantalla los usa todavía. El frontend sigue con el tenant fijo en
  `consultas.ts` y la cabecera `X-Tenant-Id`, que para las rutas públicas
  —catálogo, disponibilidad, crear reserva— sigue siendo lo correcto: no hay
  token del que derivar el negocio. Para un administrador ya no manda: su tenant
  va dentro del token.
- **El contrato cubre catálogo, disponibilidad, reservas, cuentas y la
  configuración del negocio**, incluida la auditoría de quién la cambió. Lo que
  todavía no existe es el dinero (RF-33, RF-29, RF-34), las notificaciones
  configurables (RF-16), las métricas (RF-11) y la lista de espera (RF-37).
- **El pago es Stripe**, confirmado por webhook asíncrono. El diseño del paso de
  pago en el frontend no está resuelto.
- **Sin sistema de diseño.** `index.css` solo importa Tailwind; no hay tokens,
  tipografía ni paleta. El README del frontend lo dice explícitamente: lo que
  existe es andamiaje pensado para sustituirse.
- **Superficie propia para `super_admin`**: sin decidir.

## Brand Commitments

No existe nombre de producto, logo, paleta ni identidad. "Sistema de reservas"
es una descripción, no una marca. El nombre y la identidad visual se deciden
fuera de este documento.

**Idioma: solo español (es-CO).** Una sola configuración regional por ahora, en
la línea de la semilla de desarrollo (`COP`, `America/Bogota`). Esto es una
decisión de alcance de producto, no del modelo: el modelo ya soporta moneda y
zona horaria por tenant (RF-38), así que el formato de fechas, horas y dinero se
deriva del tenant aunque la interfaz esté en un solo idioma.

## Evidence on Hand

Lo que existe de verdad y se puede usar:

- **38 requisitos funcionales** en `docs/uml/requirements/RF-01…RF-38.puml`, cada
  uno con sus actores, su flujo feliz y sus caminos de error explícitos.
- **Arquitectura y modelo de datos** en `docs/uml/arquitectura/` y
  `docs/uml/modelo-datos/`, con las decisiones justificadas en notas.
- **El contrato de la API** en `api/openapi.yaml`, del que salen los tipos de
  ambos lados.
- **El esquema físico completo** en `db/migrations/`, incluidas las restricciones
  que son el requisito.
- **Andamiaje del frontend** en `frontend/src`: enrutado, cliente tipado,
  `EstadoConsulta`, la ruta completa de reserva (catálogo → disponibilidad →
  reserva, con el 409 resuelto) y el listado de reservas del tenant, con
  pruebas.
- **Cuentas y acceso, en el backend**: alta con verificación por correo, inicio
  de sesión con contraseña y con magic link, par de tokens con refresco
  rotatorio, sesiones listables y revocables, perfil, recuperación de
  contraseña, preferencias de aviso y tokens de agente. Todo con el alcance de
  RF-23 aplicado en la consulta, no después de traerla.
- **La configuración del negocio, en el backend**: sedes, servicios y recursos
  (RF-30), reglas de disponibilidad y excepciones de calendario (RF-14),
  políticas versionadas (RF-15), vouchers (RF-17) y tarifas (RF-31), cada
  cambio con su evento de auditoría escrito en la misma transacción (RF-36).
- **Semilla de desarrollo** (`db/semillas/dev.sql`): un tenant "Estudio Demo",
  una "Sede Centro", un servicio "Sesión de una hora" a 80.000 COP, una "Sala 1".

Lo que **no** existe y no debe inventarse: clientes reales, testimonios, cifras
de uso, logos, precios comerciales del SaaS, planes, capturas de negocios reales,
casos de éxito, certificaciones o acuerdos de nivel de servicio publicados. Los
datos de la semilla son de desarrollo y no son un negocio real.

## Product Principles

1. **La verdad del cupo está en el motor, y la interfaz lo admite.** Mostrar
   disponibilidad es una promesa optimista con 2 segundos de holgura. El diseño
   trata el conflicto —el `409`— como un desenlace previsto que se resuelve con
   dignidad, no como una excepción que se disculpa.
2. **El reloj es parte del producto.** Reservar es apartar con fecha de
   caducidad. Cuánto queda, qué pasa si vence y cómo recuperar el intento son
   contenido de primer orden, no un detalle del flujo de pago.
3. **Un vocabulario, el del modelo.** Sede, servicio, recurso, política,
   voucher, lista de espera. Si un término no sirve para una persona, se cambia
   en el modelo y en la interfaz a la vez, nunca solo en la pantalla.
4. **El alcance se ve, no se supone.** Quién eres, sobre qué cuenta actúas y qué
   te está permitido tiene que ser legible en pantalla, sobre todo cuando un
   agente actúa en nombre de otro.
5. **Multi-tenant significa que la interfaz es de cada negocio.** Zona horaria,
   moneda, catálogo y políticas son del tenant. Ninguna pantalla asume las del
   navegador ni las del desarrollador.

## Accessibility & Inclusion

No se ha establecido un estándar obligatorio ni una necesidad de usuario
específica; queda como decisión abierta. Lo que sí está confirmado por el
contexto: el cliente final llega con prisa y a menudo desde el móvil, y el flujo
crítico —elegir horario y pagar— corre contra un temporizador, lo que hace del
tiempo suficiente y de la recuperación tras un fallo una cuestión de acceso, no
solo de comodidad.
