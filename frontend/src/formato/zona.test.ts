import { describe, expect, it } from 'vitest'

import { finDelDia, hora, inicioDelDia, periodo } from './zona'

// Estas pruebas fijan valores concretos en vez de recalcular con Intl lo mismo
// que calcula el código. Recalcularlo haría pasar la prueba aunque el código
// usara la zona equivocada, porque la prueba usaría esa misma zona equivocada.

describe('inicioDelDia', () => {
  it('interpreta la fecha en la zona de la sede y no en la del navegador', () => {
    // Bogotá va a -05:00 todo el año, así que su medianoche es la 05:00 UTC.
    expect(inicioDelDia('2026-09-07', 'America/Bogota').toISOString()).toBe(
      '2026-09-07T05:00:00.000Z',
    )
  })

  it('cierra la ventana en la medianoche siguiente', () => {
    expect(finDelDia('2026-09-07', 'America/Bogota').toISOString()).toBe(
      '2026-09-08T05:00:00.000Z',
    )
  })

  // La prueba que de verdad importa. Con un desfase calculado una sola vez y
  // reutilizado, una de estas dos sale con una hora de más, y el error aparece
  // solo dos veces al año: la peor forma de descubrirlo.
  it('sigue el horario de verano de la zona', () => {
    expect(inicioDelDia('2026-01-15', 'Europe/Madrid').toISOString()).toBe(
      '2026-01-14T23:00:00.000Z',
    )
    expect(inicioDelDia('2026-07-15', 'Europe/Madrid').toISOString()).toBe(
      '2026-07-14T22:00:00.000Z',
    )
  })

  it('no desplaza nada en una zona sin desfase', () => {
    expect(inicioDelDia('2026-09-07', 'UTC').toISOString()).toBe('2026-09-07T00:00:00.000Z')
  })
})

describe('presentación', () => {
  it('muestra la hora del reloj de la sede', () => {
    // Las 14:00 UTC son las 09:00 en Bogotá: la hora a la que abre la semilla.
    expect(hora('2026-09-07T14:00:00Z', 'America/Bogota')).toBe('09:00')
  })

  it('la misma reserva se lee distinta en dos sedes, y esa es la idea', () => {
    const instante = '2026-09-07T14:00:00Z'
    expect(hora(instante, 'America/Bogota')).toBe('09:00')
    expect(hora(instante, 'Europe/Madrid')).toBe('16:00')
  })

  // El período es semiabierto [inicio, fin): el instante final NO pertenece a
  // la reserva. Se muestra igualmente porque "09:00–10:00" es lo natural para
  // una persona, y es justo lo que permite que la cita siguiente empiece a las
  // 10:00.
  it('muestra el límite superior aunque no pertenezca al período', () => {
    expect(
      periodo({ inicio: '2026-09-07T14:00:00Z', fin: '2026-09-07T15:00:00Z' }, 'America/Bogota'),
    ).toBe('09:00 – 10:00')
  })
})
