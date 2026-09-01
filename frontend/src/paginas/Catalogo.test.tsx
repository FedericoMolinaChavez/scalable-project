import { screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderizar, respuestaJSON, respuestaProblema } from '../pruebas/utilidades'
import { Catalogo } from './Catalogo'

// Se simula `fetch` y no el cliente de openapi-fetch: así la prueba ejercita
// también el armado de la URL y de las cabeceras, que es donde se rompen las
// cosas al cambiar el contrato.
const fetchSimulado = vi.fn()

beforeEach(() => {
  vi.stubGlobal('fetch', fetchSimulado)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchSimulado.mockReset()
})

describe('Catálogo', () => {
  it('muestra las sedes que devuelve la API', async () => {
    fetchSimulado.mockResolvedValue(
      respuestaJSON({
        datos: [
          {
            id: '11111111-1111-1111-1111-111111111111',
            nombre: 'Sede Centro',
            zona_horaria: 'America/Bogota',
            estado: 'activo',
          },
        ],
      }),
    )

    renderizar(<Catalogo />)

    expect(await screen.findByText('Sede Centro')).toBeInTheDocument()
    expect(screen.getByText(/America\/Bogota/)).toBeInTheDocument()
  })

  it('pide /v1/sedes con la cabecera de tenant', async () => {
    fetchSimulado.mockResolvedValue(respuestaJSON({ datos: [] }))

    renderizar(<Catalogo />)

    await waitFor(() => expect(fetchSimulado).toHaveBeenCalled())

    const peticion = fetchSimulado.mock.calls[0][0] as Request
    expect(peticion.url).toContain('/v1/sedes')
    expect(peticion.headers.get('X-Tenant-Id')).toBe('11111111-1111-1111-1111-111111111111')
  })

  it('distingue el vacío del error en vez de dejar la pantalla en blanco', async () => {
    fetchSimulado.mockResolvedValue(respuestaJSON({ datos: [] }))

    renderizar(<Catalogo />)

    expect(await screen.findByText(/todavía no hay sedes/i)).toBeInTheDocument()
  })

  it('muestra el problema de la API cuando la petición falla', async () => {
    fetchSimulado.mockResolvedValue(
      respuestaProblema(500, 'Error interno', 'La base de datos no responde'),
    )

    renderizar(<Catalogo />)

    const alerta = await screen.findByRole('alert')
    expect(alerta).toHaveTextContent('Error interno')
    expect(alerta).toHaveTextContent('La base de datos no responde')
  })
})
