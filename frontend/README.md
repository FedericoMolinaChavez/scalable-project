# Frontend

React 19 + TypeScript sobre Vite.

```bash
task front:install   # npm ci
task front:dev       # servidor de desarrollo en :5173
task front:test
task front:lint
task front:build
```

Para que las llamadas a la API funcionen hace falta el backend levantado:

```bash
task infra:up
task back:run -- nucleo
```

## Estado

**Es andamiaje, no diseño.** La estructura, el enrutado, el cliente de la API y
los estados de carga y error están montados y probados. La identidad visual
—tipografía, color, espaciado, jerarquía— se define aparte. Lo que hay ahora es
lo mínimo para que la aplicación sea legible mientras tanto, y está pensado para
sustituirse.

## Estructura

```
src/
  api/
    esquema.ts          GENERADO desde api/openapi.yaml — no editar
    cliente.ts          cliente HTTP tipado y ErrorApi
    clienteConsultas.ts configuración de TanStack Query
    consultas.ts        una queryOptions por endpoint
  componentes/          reutilizables entre pantallas
  paginas/              una por ruta
  pruebas/              utilidades y preparación de Vitest
```

## Decisiones que conviene entender

**Los tipos no se escriben, se generan.** `esquema.ts` sale de
`api/openapi.yaml` con `task api:generar`. Si el contrato cambia y este código
ya no encaja, deja de compilar. Ese es el punto: la desincronización aparece al
compilar, no en producción.

**La URL base es el propio origen, nunca una URL de backend embebida.** En
desarrollo el proxy de Vite reenvía `/v1` al backend; en producción el Gateway
sirve ambos desde el mismo origen (ARQ-01). El cliente no se comporta distinto
en un sitio y en otro.

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

- Las pantallas consumen endpoints que el backend todavía no implementa; hoy
  responden `404` y la interfaz muestra el estado de error, que es lo correcto.
- Sin autenticación: el tenant va fijo en `consultas.ts`, igual que la cabecera
  provisional del contrato. Sale del token cuando exista RF-12.
