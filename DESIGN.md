# Design

<!-- impeccable:design-schema 1 -->

Sistema visual del frontend, escrito **desde lo construido**, no desde la
intención. Lo que aquí se describe existe en `frontend/src/index.css` y en
`frontend/src/componentes/cupon.tsx`; si algo diverge, manda el código y este
archivo está desactualizado.

El contrato de dirección de esta construcción vive como comentario HTML en
`frontend/index.html`, dentro del `<body>`. Seed key `9db2fb0d`.

## El mundo

**La cartera de billete de la era del jet.** Cupones de papel con cabecera de
pestañas, campos reglados a filete, cifras estampadas en carbón violeta, talón
perforado de copia carbón, y sello para lo anulado.

No es una decoración con tema. Cada pieza del objeto corresponde a una verdad
del modelo de datos, y esa correspondencia es lo que hay que preservar al
extender el sistema:

| Pieza | Qué significa en el producto |
|---|---|
| el cupón | una reserva: se emite, se sella, se anula, **nunca se borra** |
| la tira roja perforada | el reloj de RF-27: lo provisional cuelga de una tira |
| el carbón violeta | lo que el sistema ya registró y no se puede editar |
| el sello inclinado | `cancelada`, `expirada`, `no_show` — RF-28 es append-only |
| el talón de copia carbón | el comprobante de RF-34, literalmente la copia del pago |
| el tablero de salidas | la disponibilidad: filas de tramos con su estado |

La regla que gobierna la composición: **los elementos se separan con filetes,
no con tarjetas**. Un cupón sí levanta del papel —es una hoja apoyada sobre
otra— pero con una sombra corta, desplazada y difuminada, del azul de la
compañía. Lo que no existe en ninguna pantalla es una rejilla de tarjetas
flotando sobre gris.

## Color

Estrategia: **paleta completa**, cuatro papeles y cuatro tintas con papeles
asignados. Luz clara y sin modo oscuro, y es una decisión, no un olvido: la
escena real es alguien mirando el móvil en un bus a las 8:40 de la mañana en
Bogotá, con sol en la pantalla. Un cupón de papel no tiene versión nocturna.

No hay un solo gris neutro en la paleta. Los tonos secundarios se sacan del
propio azul, porque un gris al lado de una tinta de imprenta se lee como
suciedad y no como jerarquía.

| Token | Valor | Papel |
|---|---|---|
| `--color-marina` | `#0d1b3d` | estructura, texto principal, barra superior |
| `--color-marina-media` | `#465067` | secundario (7.2:1 sobre papel) |
| `--color-marina-tenue` | `#626b80` | terciario y placeholders (4.8:1) |
| `--color-rojo` | `#d7262d` | acción principal y anulación |
| `--color-rojo-hondo` | `#a81c22` | su estado pulsado |
| `--color-carbon` | `#4b2e83` | **toda cifra registrada** |
| `--color-copia` / `--color-copia-papel` | `#c7b8e6` / `#ded4f0` | el talón del comprobante |
| `--color-conforme` | `#17703f` | estado a tiempo (5.5:1) |
| `--color-papel` | `#f2f2f0` | el fondo de la aplicación |
| `--color-cupon` | `#ffffff` | el papel del cupón |
| `--color-filete` | `#c9c9c4` | el filete que separa campos |

`--color-marina-tenue` y `--color-conforme` se eligieron **por contraste**, no
por gusto: sus valores más bonitos daban 2.7:1 y 4.48:1, y el texto pequeño de
esta interfaz se lee a pleno sol.

El fondo lleva la trama de seguridad del billete: una retícula de puntos de 4 px
del azul de la compañía al 5.5%.

## Tipografía

Dos caras y ninguna más.

- **Archivo** (`--font-cuerpo`) — la grotesca de compañía aérea. Titulares en
  versalitas a peso 800 con `letter-spacing: -0.035em` y `line-height: 0.94`;
  etiquetas de campo a 10 px con `+0.12em`.
- **Azeret Mono** (`--font-datos`) — la máquina que estampa. **Toda cifra que se
  MIDE** —horas, importes, referencias, cuentas atrás, zonas horarias— y siempre
  con `tabular-nums`. Sin cifras tabulares, una cuenta atrás cambia de ancho
  cada segundo y el bloque tiembla, que es exactamente la urgencia fabricada que
  este producto rechaza.

La mono aquí no es un disfraz de «técnico»: separa el dato registrado del texto
que alguien escribió.

## Componentes

Las clases viven en `@layer components` de `index.css`; los envoltorios de React
en `componentes/cupon.tsx`.

- `.cupon` — papel blanco, contorno de filete azul, troquel en dos esquinas por
  `clip-path`, y `filter: drop-shadow(...)`. **Es `filter` y no `box-shadow` a
  propósito**: `clip-path` recorta también la sombra de caja, así que un
  `box-shadow` aquí no se vería.
- `.pestanas` / `.pestana` — la cabecera. Cada pestaña es un trapecio.
- `.campos` / `.campo` — la rejilla. El fondo de la rejilla **es** el filete y
  cada campo se recorta encima con un hueco de 1 px, para que los separadores
  sean continuos en vez de sumarse en las intersecciones.
- `.estado` — chip contorneado. La palabra va siempre; el color es el segundo
  canal.
- `.sello` — el tampón inclinado de lo anulado.
- `.tira` — la tira roja perforada de lo provisional.
- `.talon` — el papel lavanda del comprobante, con su troquel de arranque.
- `.boton` / `.boton--secundario` / `.boton--texto`.
- `.entrada` — al enfocarla pasa a carbón, el color de lo que se va a registrar.
- `.marca` — `/// ETIQUETA`. **Es el encabezado de una sección, nunca un
  antetítulo colgado encima de un `h1`.**

## Movimiento

Una curva (`--salida`, exponencial de salida) y una duración base (`--paso`,
220 ms). Dos momentos y nada más:

- `emitir` — **el único momento coreografiado**: el cupón saliendo de la
  máquina, una vez, en el instante en que el núcleo devuelve la reserva.
- `estampar` — el sello cayendo y asentándose. Dos fotogramas: un tampón no
  rebota.
- `avanzar` — el filete que se rellena: la espera de esta interfaz.

El movimiento es amortiguado y continuo por decisión de producto: la cuenta
atrás avanza sin saltos y la desviación se inclina antes de alarmar. El rojo
del reloj aparece en el último minuto, después de que la barra lleve catorce
acortándose a la vista.

`prefers-reduced-motion` deja el contenido intacto y quita el viaje.

## Lo que este sistema NO hace

- **Sin modo oscuro.** Ver arriba.
- **Sin antetítulos.** Prohibido por el suelo de oficio y quitado en la revisión.
- **Sin filetes de color al costado** de avisos o alertas. Un aviso usa el
  vocabulario del mundo: contorno, tira o papel de copia.
- **Sin iconos de librería ni glifos Unicode.** El único icono es el rótulo,
  dibujado a mano en SVG con el mismo grosor de trazo que el resto.
- **Sin nombre de producto ni logo.** PRODUCT.md dice que no existen, y la barra
  no inventa ninguno: dice «Reservas», que es una descripción.

## Pendiente

- El formulario de Stripe (`PaymentElement`) recibe la paleta y la tipografía
  por el objeto `appearance`, leído de las variables CSS en tiempo de ejecución
  (`componentes/Pago.tsx`). **Todavía no se ha visto renderizado**: hace falta
  una clave publicable de prueba real.
- Las superficies del administrador y del agente no existen. Cuando lleguen,
  heredan este mundo: la agenda es el mismo tablero a escala de semana, y el
  «actuando en nombre de» es el nombre escrito arriba de un cupón.
