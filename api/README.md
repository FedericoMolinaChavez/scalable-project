# Contrato de la API

El contrato es la fuente de verdad entre el backend en Go y el frontend en
React. Ninguno de los dos escribe a mano los tipos de la frontera: los dos los
generan desde aquí.

```bash
task api:validar    # comprueba que es OpenAPI válido
task api:generar    # empaqueta y regenera ambos lados
task api:verificar  # falla si lo generado no coincide con el contrato
```

## Estructura

```
openapi.yaml              raíz: solo el mapa de rutas
rutas/
  catalogo.yaml           sedes, servicios
  disponibilidad.yaml
  reservas.yaml           colección y detalle
componentes/
  parametros.yaml         Tenant
  respuestas.yaml         errores comunes
  esquemas/
    comunes.yaml          Problema, Periodo, Dinero
    catalogo.yaml         Sede, Servicio
    disponibilidad.yaml   Franja, Disponibilidad
    reservas.yaml         Reserva, NuevaReserva, ...
openapi.bundled.yaml      GENERADO: los anteriores con los $ref resueltos
redocly.yaml              configuración del validador
```

Está partido porque OpenAPI obliga a que los esquemas vivan en `components`,
lejos de la ruta que los usa. En un archivo único, seguir un solo endpoint
obliga a saltar entre dos zonas separadas por cientos de líneas, y eso no se
arregla reordenando. Con esta división, leer un endpoint es abrir un archivo.

**No edites `openapi.bundled.yaml`.** Se regenera y cualquier cambio a mano se
pierde en el siguiente `task api:generar`.

## Por qué hay un paso de empaquetado

Porque `oapi-codegen` **no resuelve `$ref` a archivos externos**: falla pidiendo
`--import-mapping`, que sirve para mapear specs distintos a paquetes Go
distintos, no para partir uno solo por legibilidad.

`redocly bundle` resuelve las referencias en un documento único con refs
internos, que es lo que el generador sí entiende. El empaquetado se versiona y
`api:verificar` comprueba que está al día, igual que con el resto de artefactos
generados. De paso queda un archivo resuelto que se puede importar en Postman
sin instalar nada.

## Por qué generar y no escribir

Un contrato escrito a mano en dos sitios se desincroniza, y el momento en que se
nota es cuando el frontend ya está desplegado y una respuesta no tiene el campo
que esperaba. Generando, romper el contrato rompe la **compilación** de Go y de
TypeScript, que es cuando sale barato.

`api:verificar` está en `task ci` justamente para quien edita el contrato y
olvida regenerar: sin esa comprobación, el contrato y el código dirían cosas
distintas sin que nada lo notara.

| Destino | Herramienta | Salida |
|---|---|---|
| Go | `oapi-codegen` | `backend/internal/api/api.gen.go` — modelos y `StrictServerInterface` |
| TypeScript | `openapi-typescript` | `frontend/src/api/esquema.ts` — solo tipos |

El generador de Go está fijado en `backend/go.mod` con la directiva `tool`, así
que su versión viaja con el repositorio.

La interfaz de Go es *estricta*: los manejadores devuelven `(respuesta, error)`
tipados en vez de escribir en el `ResponseWriter`. El compilador pasa a
garantizar que cada ruta solo devuelve los códigos que el contrato declara.

## Cuatro decisiones que conviene entender

**Los períodos son `[inicio, fin)`.** Con ambos límites cerrados, 10:00–11:00 y
11:00–12:00 comparten un instante, `&&` las declara solapadas y la restricción
EXCLUDE rechazaría dos citas consecutivas válidas. Es la misma regla que impone
`db/README.md`, y el contrato la repite para que el cliente no la invente.

**El dinero va como cadena, no como número.** En JSON los números son de coma
flotante y `80000.10` no se representa exacto. La columna del esquema es
`numeric`; una cadena preserva esa exactitud de extremo a extremo.

**`409` en `POST /v1/reservas` no es un error.** Es el resultado normal de
perder una carrera por el mismo cupo: otra transacción se quedó con él y la
restricción EXCLUDE rechazó esta. El cliente debe ofrecer otra franja, no
reintentar la misma.

**`Idempotency-Key` es obligatoria.** Un agente que reintenta un `POST` tras un
timeout crearía dos bloqueos que nadie libera hasta el TTL.

## Pendiente

**No hay autenticación.** El validador lo señala en cada ejecución con seis
avisos de `security-defined`, y está degradado a aviso —no silenciado— a
propósito: debe seguir apareciendo hasta que RF-12 exista. Declarar ahora un
esquema Bearer sería peor que no tenerlo, porque el contrato prometería una
autenticación que el servidor no comprueba.

Mientras tanto el tenant llega en `X-Tenant-Id`. Aceptar esa cabecera de un
cliente en producción permitiría leer los datos de cualquier otro tenant.
