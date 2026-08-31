/**
 * Fechas y horas en la zona del negocio, nunca en la del navegador.
 *
 * No es una preferencia de formato. Un tenant define su zona horaria y cada
 * sede la suya (RF-38), y una reserva de las 10:00 significa las 10:00 del
 * reloj de la sede. Mostrarla convertida a la hora del visitante haría que la
 * misma reserva se leyera distinta según desde dónde se mire, y quien viaja
 * vería su cita moverse sola.
 *
 * El backend siempre habla en instantes UTC. La zona entra solo al presentar.
 */

/**
 * Desfase de una zona respecto a UTC, en minutos, para un instante concreto.
 *
 * Para un instante y no para la zona en general: en una zona con horario de
 * verano el desfase cambia dos veces al año, y calcularlo una vez y reutilizarlo
 * produce un error de una hora justo alrededor del cambio.
 */
export function desfaseMinutos(instante: Date, zona: string): number {
  const partes = new Intl.DateTimeFormat('en-US', {
    timeZone: zona,
    timeZoneName: 'longOffset',
  }).formatToParts(instante)

  const nombre = partes.find((parte) => parte.type === 'timeZoneName')?.value ?? ''

  // 'longOffset' produce "GMT-05:00", y "GMT" a secas cuando el desfase es cero.
  const coincidencia = /GMT([+-])(\d{2}):(\d{2})/.exec(nombre)
  if (!coincidencia) return 0

  const signo = coincidencia[1] === '-' ? -1 : 1
  return signo * (Number(coincidencia[2]) * 60 + Number(coincidencia[3]))
}

/**
 * El instante en que empieza un día local de la sede.
 *
 * `fecha` es un `YYYY-MM-DD` tal como lo escribe un `<input type="date">`, y se
 * interpreta en la zona de la sede, no en la del navegador. Es lo que hace que
 * "el 7 de septiembre" signifique lo mismo para quien reserva desde Bogotá y
 * para quien lo hace desde Madrid.
 */
export function inicioDelDia(fecha: string, zona: string): Date {
  // Primera aproximación: medianoche UTC de esa fecha. Sirve para preguntar qué
  // desfase tenía la zona ese día, que es lo que hace falta para corregirla.
  const aproximado = new Date(`${fecha}T00:00:00Z`)
  return new Date(aproximado.getTime() - desfaseMinutos(aproximado, zona) * 60_000)
}

/** El día siguiente, para cerrar la ventana `[desde, hasta)`. */
export function finDelDia(fecha: string, zona: string): Date {
  const siguiente = new Date(`${fecha}T00:00:00Z`)
  siguiente.setUTCDate(siguiente.getUTCDate() + 1)

  const texto = siguiente.toISOString().slice(0, 10)
  return inicioDelDia(texto, zona)
}

/** Solo la hora, para una franja: "10:00". */
export function hora(instante: string, zona: string): string {
  return new Intl.DateTimeFormat('es', {
    timeZone: zona,
    hour: '2-digit',
    minute: '2-digit',
  }).format(new Date(instante))
}

/** Fecha y hora completas, para una reserva ya creada. */
export function fechaYHora(instante: string, zona: string): string {
  return new Intl.DateTimeFormat('es', {
    timeZone: zona,
    dateStyle: 'medium',
    timeStyle: 'short',
  }).format(new Date(instante))
}

/**
 * El período de una franja u reserva, tal como lo lee una persona.
 *
 * Se muestra "10:00–11:00" aunque el intervalo sea semiabierto `[inicio, fin)`
 * y el instante final NO pertenezca a la reserva. Es lo natural de leer, y a la
 * vez es justo lo que permite que la cita siguiente empiece a las 11:00: ningún
 * cálculo debe tratar `fin` como ocupado.
 */
export function periodo(
  franja: { inicio: string; fin: string },
  zona: string,
  conFecha = false,
): string {
  const inicio = conFecha ? fechaYHora(franja.inicio, zona) : hora(franja.inicio, zona)
  return `${inicio} – ${hora(franja.fin, zona)}`
}

/** El día de hoy en la zona de la sede, en el formato de un `<input type="date">`. */
export function hoyEn(zona: string): string {
  return new Intl.DateTimeFormat('en-CA', {
    timeZone: zona,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(new Date())
}
