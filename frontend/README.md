# Frontend

React 19 + TypeScript sobre Vite.

```bash
task front:install   # npm ci
task front:dev       # servidor de desarrollo en :5173
task front:test
task front:lint
task front:build
```

Para que las llamadas a la API funcionen hacen falta los **tres** servicios del
backend levantados:

```bash
task infra:up
task back:run -- consulta    # :8081 — lecturas
task back:run -- nucleo      # :8080 — escrituras
task back:run -- identidad   # :8082 — códigos y tokens
```

El código de «Mis reservas» llega a Mailpit: <http://localhost:8025>.

## Estado

**Es andamiaje, no diseño.** La estructura, el enrutado, el cliente de la API,
los estados de carga y error y la ruta completa de reserva están montados y
probados. La identidad visual —tipografía, color, espaciado, jerarquía— se
define aparte. Lo que hay ahora es lo mínimo para que la aplicación sea legible
mientras tanto, y está pensado para sustituirse.

`Reservar` es la rebanada vertical completa: de ese componente al servicio de
disponibilidad, de ahí al núcleo y de ahí a la restricción EXCLUDE de
PostgreSQL. Lo que demuestra no es la pantalla, es que las cuatro capas encajan.

## Estructura

```
src/
  api/
    esquema.ts          GENERADO desde api/openapi.yaml — no editar
    cliente.ts          cliente HTTP tipado y ErrorApi
    clienteConsultas.ts configuración de TanStack Query
    consultas.ts        una queryOptions por endpoint (lecturas)
    mutaciones.ts       la única escritura, y las claves de idempotencia
  componentes/          reutilizables entre pantallas
  formato/              presentación en la zona horaria de la sede
  paginas/              una por ruta
  pruebas/              utilidades y preparación de Vitest
```

La escritura vive aparte de las lecturas por la misma razón que en el backend
(ARQ-01): tiene garantías, reintentos y modos de fallo propios, y mezclarla con
las lecturas invita a tratarla igual que ellas.

## Decisiones que conviene entender

**Los tipos no se escriben, se generan.** `esquema.ts` sale de
`api/openapi.yaml` con `task api:generar`. Si el contrato cambia y este código
ya no encaja, deja de compilar. Ese es el punto: la desincronización aparece al
compilar, no en producción.

**La URL base es el propio origen, nunca una URL de backend embebida.** En
desarrollo el proxy de Vite reenvía `/v1` al backend; en producción el Gateway
sirve ambos desde el mismo origen (ARQ-01). El cliente no se comporta distinto
en un sitio y en otro.

**El proxy enruta por método, no solo por ruta.** `/v1/reservas` lo sirven dos
procesos: el `POST` el núcleo (:8080) y el `GET` el de consulta (:8081). Es la
frontera de ARQ-01, y el proxy de Vite no sabe expresarla con configuración, así
que un plugin de desarrollo reescribe la URL de las escrituras antes de que el
proxy la vea. Está comentado en `vite.config.ts`. No afecta a `vite build`.

**Las horas se muestran en la zona de la SEDE, nunca en la del navegador.** Un
tenant define su zona y cada sede la suya (RF-38): una cita de las 10:00 es a
las 10:00 del reloj del negocio. Convertirla a la hora del visitante haría que
la misma reserva se leyera distinta según desde dónde se mire, y quien viaja
vería su cita moverse sola. El backend siempre habla en instantes UTC; la zona
entra solo al presentar (`src/formato/zona.ts`).

**La clave de idempotencia va atada a la franja, no al clic.** Tiene que ser la
misma cuando alguien reintenta el mismo horario —si no, el reintento crea un
segundo bloqueo que nadie libera hasta que venza su TTL— y distinta cuando elige
otro. Tras un 409 se olvida, porque quedó atada a un intento que nunca creó
nada.

**No se reintenta ningún 4xx.** Un `409` significa que otra transacción se quedó
con el cupo (RF-01, alt. 3): reintentar es insistir en algo que ya no está
disponible, y multiplica la carga sobre el núcleo justo cuando hay contención.
Solo se reintentan los 5xx y los fallos de red, dos veces.

**La disponibilidad se cachea 2 segundos**, que es exactamente la
desactualización que RNF-10 autoriza. Es una lectura optimista: puede mostrar un
cupo que otro acaba de tomar, y quien decide es el núcleo al insertar.

**`EstadoConsulta` obliga a los tres estados.** Cargando, error y vacío. Escrito
a mano en cada pantalla, el que siempre falta es el de error, y la interfaz se
queda en blanco sin explicar nada.

## Una rareza del cliente HTTP

`cliente.ts` pasa `fetch: (peticion) => globalThis.fetch(peticion)` en vez de
dejar que openapi-fetch use el global directamente. No es adorno: openapi-fetch
captura `globalThis.fetch` al **crear** el cliente, y este cliente es un
singleton de módulo que se construye al importarlo. Con la referencia directa,
sustituir el global en una prueba llega tarde y las peticiones se van a la red
de verdad.

Si el simulado de peticiones crece, la alternativa es MSW, que intercepta a
nivel de red y no obliga a esta indirección.

## Pendiente

- **Sin autenticación**: el tenant va fijo en `consultas.ts`, igual que la
  cabecera provisional del contrato. Sale del token cuando exista RF-12.
- **Sin agenda del administrador.** `GET /v1/reservas` acota por quien presenta
  el token (RF-23), y hoy solo existe el de invitado. La agenda del negocio
  (RF-32) es otra superficie —por sede y recurso, con acciones sobre cada
  reserva— y llega con RF-12.
- **La sesión del cliente muere con la pestaña**: el token vive en
  `sessionStorage`. Caduca en minutos y no se puede revocar, así que
  sobrevivir al cierre solo alargaría la ventana en la que sirve de algo a quien
  lo robe.
- **Sin pago**: el `201` deja la reserva pendiente y el temporizador corriendo,
  pero no hay paso de pago detrás. Llega con Stripe (RF-33).
- **Sin cancelar ni modificar** (RF-06, RF-07), sin detalle de reserva (RF-03) y
  sin paginación en la lista, aunque el contrato y el backend ya la sirven.
