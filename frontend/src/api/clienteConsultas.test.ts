import { describe, expect, it } from 'vitest'

import { ErrorApi } from './cliente'
import { crearClienteConsultas } from './clienteConsultas'

describe('política de reintentos', () => {
  const reintentar = (intentos: number, error: Error) => {
    const politica = crearClienteConsultas().getDefaultOptions().queries?.retry
    if (typeof politica !== 'function') throw new Error('se esperaba una función de reintento')
    return politica(intentos, error)
  }

  // El caso que motiva la política. Un 409 significa que otra transacción se
  // quedó con el cupo (RF-01, alt. 3): reintentar es insistir en algo que ya
  // no está disponible, y encima multiplica la carga sobre el núcleo justo
  // cuando hay contención.
  it('no reintenta un 409, porque el cupo ya lo tomó otro', () => {
    expect(reintentar(0, new ErrorApi(409))).toBe(false)
  })

  it('no reintenta ningún 4xx', () => {
    expect(reintentar(0, new ErrorApi(400))).toBe(false)
    expect(reintentar(0, new ErrorApi(404))).toBe(false)
    expect(reintentar(0, new ErrorApi(422))).toBe(false)
  })

  it('reintenta los 5xx, que sí pueden ser transitorios', () => {
    expect(reintentar(0, new ErrorApi(503))).toBe(true)
  })

  it('deja de reintentar tras dos intentos', () => {
    expect(reintentar(2, new ErrorApi(503))).toBe(false)
  })

  it('reintenta los fallos de red, que no traen estado', () => {
    expect(reintentar(0, new TypeError('Failed to fetch'))).toBe(true)
  })
})

describe('ErrorApi', () => {
  it('identifica el horario ocupado', () => {
    expect(new ErrorApi(409).esHorarioOcupado).toBe(true)
    expect(new ErrorApi(500).esHorarioOcupado).toBe(false)
  })

  it('usa el título del problema como mensaje cuando existe', () => {
    const error = new ErrorApi(409, {
      type: 'about:blank',
      title: 'El horario ya está reservado',
      status: 409,
    })
    expect(error.message).toBe('El horario ya está reservado')
  })

  it('cae en un mensaje genérico si la respuesta no trae cuerpo', () => {
    expect(new ErrorApi(502).message).toContain('502')
  })
})
