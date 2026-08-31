import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { render, type RenderOptions } from '@testing-library/react'
import type { ReactElement, ReactNode } from 'react'
import { MemoryRouter } from 'react-router'

/**
 * Renderiza con los proveedores que la aplicación real monta.
 *
 * Cada prueba recibe un QueryClient nuevo: compartirlo haría que una prueba
 * viera en caché los datos que pidió otra, y el resultado dependería del orden.
 *
 * `retry: false` es imprescindible. Con los reintentos por defecto, una prueba
 * de error espera a que se agoten antes de mostrar el estado de fallo, y acaba
 * en un timeout que parece un fallo de la interfaz y no de la configuración.
 */
export function renderizar(
  elemento: ReactElement,
  { ruta = '/', ...opciones }: RenderOptions & { ruta?: string } = {},
) {
  const cliente = new QueryClient({
    defaultOptions: {
      queries: { retry: false, staleTime: 0 },
      mutations: { retry: false },
    },
  })

  function Envoltura({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={cliente}>
        <MemoryRouter initialEntries={[ruta]}>{children}</MemoryRouter>
      </QueryClientProvider>
    )
  }

  return render(elemento, { wrapper: Envoltura, ...opciones })
}

/** Respuesta JSON para `fetch` simulado. */
export function respuestaJSON(cuerpo: unknown, status = 200): Response {
  return new Response(JSON.stringify(cuerpo), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

/** Respuesta de error con el formato RFC 9457 que usa la API. */
export function respuestaProblema(status: number, title: string, detail?: string): Response {
  return new Response(JSON.stringify({ type: 'about:blank', title, status, detail }), {
    status,
    headers: { 'Content-Type': 'application/problem+json' },
  })
}
