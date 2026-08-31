import tailwind from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
// De vitest/config y no de vite: el defineConfig de Vite no conoce la clave
// `test` y TypeScript la rechazaría.
import { defineConfig, type Plugin } from 'vitest/config'

// Los dos procesos del backend en desarrollo. En producción no existen: el
// Gateway sirve todo desde el mismo origen (ARQ-01) y enruta por su cuenta.
const NUCLEO = 'http://localhost:8080' // escritura
const CONSULTA = 'http://localhost:8081' // lectura, catálogo y disponibilidad

// Prefijo interno para las escrituras. No sale al backend: el proxy lo quita
// antes de reenviar.
const ESCRITURA = '/__escritura'

/**
 * Manda las escrituras al núcleo y todo lo demás al de consulta.
 *
 * Hace falta porque `/v1/reservas` lo sirven DOS procesos: el `POST` lo atiende
 * el núcleo y el `GET` el de consulta. Es la frontera de ARQ-01 —lo que debe
 * ser atómico con la reserva contra lo que solo lee—, y el proxy de Vite enruta
 * por ruta, no por método, así que no puede expresarla solo con configuración.
 *
 * La salida es reescribir la URL antes de que el proxy la vea: un `POST` a
 * `/v1/reservas` pasa a `/__escritura/v1/reservas`, que ya es una ruta distinta
 * y sí se puede enrutar. El prefijo se quita al reenviar, así que el backend
 * recibe la ruta del contrato y no se entera de nada.
 *
 * El middleware se registra dentro de `configureServer` y no en la función que
 * devuelve: así entra ANTES de los middlewares internos de Vite, y por tanto
 * antes del proxy. Registrado después, el proxy ya habría decidido.
 *
 * Esto es solo del servidor de desarrollo. `vite build` no lo ejecuta.
 */
function enrutarEscrituras(): Plugin {
  return {
    name: 'reservas:enrutar-escrituras',
    configureServer(servidor) {
      servidor.middlewares.use((peticion, _respuesta, siguiente) => {
        if (peticion.method === 'POST' && peticion.url?.startsWith('/v1/')) {
          peticion.url = ESCRITURA + peticion.url
        }
        siguiente()
      })
    },
  }
}

export default defineConfig({
  plugins: [react(), tailwind(), enrutarEscrituras()],

  server: {
    port: 5173,

    // El frontend llama a /v1/... como si fuera del mismo origen y Vite lo
    // reenvía. Así no hace falta configurar CORS en desarrollo ni meter la URL
    // del backend en el código: en producción el Gateway sirve ambos desde el
    // mismo origen (ARQ-01), y el cliente HTTP no tiene que comportarse
    // distinto en un sitio y en otro.
    proxy: {
      // Primero el más específico: las escrituras ya reescritas.
      [`${ESCRITURA}/v1`]: {
        target: NUCLEO,
        changeOrigin: true,
        rewrite: (ruta) => ruta.replace(ESCRITURA, ''),
      },
      '/v1': {
        target: CONSULTA,
        changeOrigin: true,
      },
    },
  },

  test: {
    environment: 'jsdom',
    globals: true,
    setupFiles: ['./src/pruebas/preparacion.ts'],
    css: false,
    coverage: {
      provider: 'v8',
      // El esquema es código generado desde api/openapi.yaml: medir su
      // cobertura no dice nada sobre las pruebas de este repositorio.
      exclude: ['src/api/esquema.ts', '**/*.config.*', 'dist/**'],
    },
  },
})
