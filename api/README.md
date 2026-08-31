# Contrato de la API

`openapi.yaml` es la fuente de verdad entre el backend en Go y el frontend en
React. Ninguno de los dos escribe a mano los tipos de la frontera: los dos los
generan desde aquí.

```bash
task api:generar    # regenera ambos lados
task api:verificar  # falla si lo generado no coincide con el contrato
```

## Por qué generar y no escribir

Un contrato escrito a mano en dos sitios se desincroniza, y el momento en que
se nota es cuando el frontend ya está desplegado y una respuesta no tiene el
campo que esperaba. Generando, romper el contrato rompe la **compilación** de
Go y de TypeScript, que es cuando sale barato.

`api:verificar` está en `task ci` justamente para el caso de quien edita
`openapi.yaml` y olvida regenerar: sin esa comprobación, el contrato y el
código dirían cosas distintas sin que nada lo notara.

## Qué genera cada lado

| Destino | Herramienta | Salida |
|---|---|---|
| Go | `oapi-codegen` | `backend/internal/api/api.gen.go` — modelos y `StrictServerInterface` |
| TypeScript | `openapi-typescript` | `frontend/src/api/esquema.ts` — solo tipos |

El generador de Go está fijado en `backend/go.mod` con la directiva `tool`, así
que su versión viaja con el repositorio y no depende de lo que cada máquina
tenga instalado.

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

`X-Tenant-Id` es provisional. En cuanto exista autenticación (RF-12) el tenant
se deriva del token y la cabecera desaparece: aceptarla de un cliente en
producción permitiría leer los datos de cualquier otro tenant.
