import { queryOptions } from '@tanstack/react-query'

import { cliente, ErrorApi } from './cliente'

/**
 * Tenant de desarrollo. Provisional, igual que la cabecera que lo transporta:
 * cuando exista autenticación (RF-12) saldrá del token y esto desaparece.
 */
const TENANT = '11111111-1111-1111-1111-111111111111'

const cabeceras = { 'X-Tenant-Id': TENANT }

/**
 * Claves de caché, centralizadas.
 *
 * Escribirlas sueltas en cada componente es la forma habitual de que una
 * invalidación no acierte: basta con que un componente use `['sedes']` y otro
 * `['sedes', undefined]` para que refrescar deje de funcionar sin dar error.
 */
export const claves = {
  sedes: () => ['sedes'] as const,
  servicios: (sedeId?: string) => ['servicios', sedeId ?? null] as const,
  reservas: () => ['reservas'] as const,
  reserva: (id: string) => ['reservas', id] as const,
  disponibilidad: (servicioId: string, desde: string, hasta: string) =>
    ['disponibilidad', servicioId, desde, hasta] as const,
}

export const consultaSedes = () =>
  queryOptions({
    queryKey: claves.sedes(),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/sedes', {
        params: { header: cabeceras },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data.datos
    },
  })

export const consultaServicios = (sedeId?: string) =>
  queryOptions({
    queryKey: claves.servicios(sedeId),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/servicios', {
        params: {
          header: cabeceras,
          query: sedeId ? { sede_id: sedeId } : {},
        },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data.datos
    },
  })

export const consultaDisponibilidad = (servicioId: string, desde: string, hasta: string) =>
  queryOptions({
    queryKey: claves.disponibilidad(servicioId, desde, hasta),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/disponibilidad', {
        params: {
          header: cabeceras,
          query: { servicio_id: servicioId, desde, hasta },
        },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data
    },

    // Los 2 s que RNF-10 autoriza como desactualización, aquí literalmente.
    // La disponibilidad es una lectura optimista: puede mostrar un cupo que
    // otro acaba de tomar, y quien decide es el núcleo al insertar. Cachear
    // más tiempo empeoraría esa ventana sin ganar nada.
    staleTime: 2_000,
  })

export const consultaReservas = () =>
  queryOptions({
    queryKey: claves.reservas(),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/reservas', {
        params: { header: cabeceras },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data.datos
    },
  })

export const consultaReserva = (id: string) =>
  queryOptions({
    queryKey: claves.reserva(id),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/reservas/{id}', {
        params: { header: cabeceras, path: { id } },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data
    },
  })
