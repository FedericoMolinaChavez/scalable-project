import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { renderizar, respuestaJSON, respuestaProblema } from '../pruebas/utilidades'
import { Reservar } from './Reservar'

// Se simula `fetch` y no el cliente de openapi-fetch: así la prueba ejercita
// también el armado de la URL y de las cabeceras, que es donde se rompen las
// cosas al cambiar el contrato. Aquí importa el doble, porque la cabecera
// Idempotency-Key es obligatoria y su ausencia solo se vería en producción.

const SEDE = {
  id: '22222222-2222-2222-2222-222222222222',
  nombre: 'Sede Centro',
  zona_horaria: 'America/Bogota',
  estado: 'activo',
}

const SERVICIO = {
  id: '33333333-3333-3333-3333-333333333333',
  sede_id: SEDE.id,
  nombre: 'Sesión de una hora',
  duracion_min: 60,
  precio: { monto: '80000.00', moneda: 'COP' },
  estado: 'activo',
}

const RECURSO = '44444444-4444-4444-4444-444444444444'

// 14:00 UTC son las 09:00 en Bogotá, que es cuando abre la sede sembrada.
const FRANJA = {
  recurso_id: RECURSO,
  periodo: { inicio: '2026-09-07T14:00:00Z', fin: '2026-09-07T15:00:00Z' },
}

const fetchSimulado = vi.fn()

/** Las peticiones que se hicieron, ya tipadas: mock.calls es any[][]. */
function peticiones(): Request[] {
  return fetchSimulado.mock.calls.map((llamada) => llamada[0] as Request)
}

/** Enruta cada petición simulada según su URL, como haría el backend real. */
function responder(porRuta: {
  disponibilidad?: () => Response
  reserva?: () => Response
}) {
  fetchSimulado.mockImplementation((peticion: Request) => {
    const url = new URL(peticion.url)

    if (url.pathname === '/v1/sedes') return Promise.resolve(respuestaJSON({ datos: [SEDE] }))
    if (url.pathname === '/v1/servicios') {
      return Promise.resolve(respuestaJSON({ datos: [SERVICIO] }))
    }
    if (url.pathname === '/v1/disponibilidad') {
      return Promise.resolve(
        porRuta.disponibilidad?.() ??
          respuestaJSON({
            servicio_id: SERVICIO.id,
            calculada_en: '2026-09-01T00:00:00Z',
            franjas: [FRANJA],
          }),
      )
    }
    if (url.pathname === '/v1/reservas') {
      return Promise.resolve(porRuta.reserva?.() ?? respuestaJSON({}, 500))
    }

    return Promise.resolve(respuestaJSON({}, 404))
  })
}

/** La reserva pendiente que devuelve el núcleo tras un 201. */
function reservaCreada() {
  return respuestaJSON(
    {
      reserva: {
        id: '66666666-6666-6666-6666-666666666666',
        servicio_id: SERVICIO.id,
        recurso_id: RECURSO,
        periodo: FRANJA.periodo,
        estado: 'pendiente',
        expira_en: new Date(Date.now() + 15 * 60_000).toISOString(),
        precio_cobrado: { monto: '80000.00', moneda: 'COP' },
        creada_en: new Date().toISOString(),
      },
    },
    201,
  )
}

/** Rellena el contacto, que es lo que habilita los botones de franja. */
async function rellenarContacto(usuario: ReturnType<typeof userEvent.setup>) {
  await usuario.type(screen.getByLabelText('Nombre'), 'Ana Prueba')
  await usuario.type(screen.getByLabelText('Correo'), 'ana@ejemplo.test')
}

beforeEach(() => {
  vi.stubGlobal('fetch', fetchSimulado)
})

afterEach(() => {
  vi.unstubAllGlobals()
  fetchSimulado.mockReset()
})

describe('Reservar', () => {
  it('pide la disponibilidad del día LOCAL de la sede, no del navegador', async () => {
    responder({})
    renderizar(<Reservar />)

    await waitFor(() => {
      const llamadas = peticiones().map((p) => new URL(p.url))
      expect(llamadas.some((u) => u.pathname === '/v1/disponibilidad')).toBe(true)
    })

    const url = peticiones()
      .map((p) => new URL(p.url))
      .find((u) => u.pathname === '/v1/disponibilidad')!

    // La medianoche de Bogotá es la 05:00 UTC. Si la ventana empezara a las
    // 00:00Z, el primer día mostraría las franjas del día anterior por la
    // tarde y se comería las últimas del propio día.
    expect(url.searchParams.get('desde')).toMatch(/T05:00:00\.000Z$/)
    expect(url.searchParams.get('hasta')).toMatch(/T05:00:00\.000Z$/)
  })

  it('muestra las franjas en la hora de la sede', async () => {
    responder({})
    renderizar(<Reservar />)

    // 14:00 UTC → 09:00 en Bogotá. Si se usara la hora del navegador, este
    // texto sería otro en cualquier máquina que no esté en Colombia.
    expect(await screen.findByRole('button', { name: '09:00 – 10:00' })).toBeInTheDocument()
    expect(screen.getByText(/America\/Bogota/)).toBeInTheDocument()
  })

  it('no deja reservar sin nombre y correo', async () => {
    responder({})
    renderizar(<Reservar />)

    const boton = await screen.findByRole('button', { name: '09:00 – 10:00' })
    expect(boton).toBeDisabled()
    expect(screen.getByText(/completa tu nombre y tu correo/i)).toBeInTheDocument()
  })

  it('manda la clave de idempotencia al crear la reserva', async () => {
    const usuario = userEvent.setup()
    responder({ reserva: reservaCreada })
    renderizar(<Reservar />)

    await screen.findByRole('button', { name: '09:00 – 10:00' })
    await rellenarContacto(usuario)
    await usuario.click(screen.getByRole('button', { name: '09:00 – 10:00' }))

    await waitFor(() => {
      const post = peticiones()
        .find((p) => p.method === 'POST')
      expect(post).toBeDefined()

      // Obligatoria, no opcional: sin ella un reintento tras un timeout crearía
      // un segundo bloqueo que nadie libera hasta que venza su TTL.
      expect(post!.headers.get('Idempotency-Key')).toBeTruthy()
      expect(post!.headers.get('X-Tenant-Id')).toBe('11111111-1111-1111-1111-111111111111')
    })
  })

  it('confirma el horario apartado y dice que el bloqueo caduca', async () => {
    const usuario = userEvent.setup()
    responder({ reserva: reservaCreada })
    renderizar(<Reservar />)

    await screen.findByRole('button', { name: '09:00 – 10:00' })
    await rellenarContacto(usuario)
    await usuario.click(screen.getByRole('button', { name: '09:00 – 10:00' }))

    const confirmacion = await screen.findByRole('status')
    expect(confirmacion).toHaveTextContent(/horario apartado/i)
    expect(confirmacion).toHaveTextContent('pendiente')

    // El bloqueo ES la reserva pendiente (RF-27). Que caduque no es un detalle
    // del flujo de pago: es lo que decide si el cupo sigue siendo suyo.
    expect(confirmacion).toHaveTextContent(/para completar el pago/i)
  })

  // El caso que justifica toda la arquitectura, visto desde la interfaz.
  it('explica el 409 sin reintentarlo', async () => {
    const usuario = userEvent.setup()
    responder({
      reserva: () =>
        respuestaProblema(
          409,
          'El horario ya está reservado',
          'Otra reserva se quedó con ese horario.',
        ),
    })
    renderizar(<Reservar />)

    await screen.findByRole('button', { name: '09:00 – 10:00' })
    await rellenarContacto(usuario)
    await usuario.click(screen.getByRole('button', { name: '09:00 – 10:00' }))

    const alerta = await screen.findByRole('alert')
    expect(alerta).toHaveTextContent('El horario ya está reservado')

    // Reintentar un 409 es insistir en un cupo que ya no está, y multiplica la
    // carga sobre el núcleo justo cuando hay contención (RF-01, alt. 3).
    const escrituras = peticiones().filter((p) => p.method === "POST")
    expect(escrituras).toHaveLength(1)
  })

  it('vuelve a pedir la disponibilidad tras un 409, porque acaba de quedar obsoleta', async () => {
    const usuario = userEvent.setup()
    responder({
      reserva: () => respuestaProblema(409, 'El horario ya está reservado'),
    })
    renderizar(<Reservar />)

    await screen.findByRole('button', { name: '09:00 – 10:00' })
    const consultasAntes = () =>
      peticiones().filter((p) => new URL(p.url).pathname === "/v1/disponibilidad").length

    const antes = consultasAntes()
    await rellenarContacto(usuario)
    await usuario.click(screen.getByRole('button', { name: '09:00 – 10:00' }))

    await screen.findByRole('alert')
    await waitFor(() => expect(consultasAntes()).toBeGreaterThan(antes))
  })

  it('distingue un día sin huecos de un fallo', async () => {
    responder({
      disponibilidad: () =>
        respuestaJSON({
          servicio_id: SERVICIO.id,
          calculada_en: '2026-09-01T00:00:00Z',
          franjas: [],
        }),
    })
    renderizar(<Reservar />)

    expect(await screen.findByText(/no quedan horarios libres/i)).toBeInTheDocument()
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
  })

  it('muestra el problema cuando la disponibilidad falla', async () => {
    responder({
      disponibilidad: () => respuestaProblema(500, 'Error interno', 'La base no responde'),
    })
    renderizar(<Reservar />)

    const alerta = await screen.findByRole('alert')
    expect(alerta).toHaveTextContent('Error interno')
  })
})
