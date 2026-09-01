import { QueryClient } from '@tanstack/react-query'

import { ErrorApi } from './cliente'

/**
 * Configuración de TanStack Query compartida por la aplicación y las pruebas.
 *
 * Vive fuera de Aplicacion.tsx porque exportar algo que no sea un componente
 * junto a los componentes rompe el fast refresh de Vite: al editar el archivo,
 * React remonta el árbol entero en vez de refrescar en caliente.
 */
export function crearClienteConsultas(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        // Reintentar un 4xx es inútil: la petición está mal y volverá a
        // estarlo. Reintentar un 409 es peor que inútil, porque significa
        // insistir en un cupo que otro ya tomó (RF-01, alt. 3) y multiplicar
        // la carga sobre el núcleo justo cuando hay contención. Solo se
        // reintentan los 5xx y los fallos de red, que sí pueden ser
        // transitorios.
        retry: (intentos, error) => {
          if (error instanceof ErrorApi && error.status < 500) return false
          return intentos < 2
        },
        staleTime: 30_000,
      },
    },
  })
}
