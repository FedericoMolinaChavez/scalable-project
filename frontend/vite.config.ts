import tailwind from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
// De vitest/config y no de vite: el defineConfig de Vite no conoce la clave
// `test` y TypeScript la rechazaría.
import { defineConfig, type Plugin } from 'vitest/config'

// Los procesos del backend en desarrollo. En producción no existen: el Gateway
// sirve todo desde el mismo origen (ARQ-01) y enruta por su cuenta.
const NUCLEO = 'http://localhost:8080' // escritura de reservas
const CONSULTA = 'http://localhost:8081' // lectura de reservas y disponibilidad
const IDENTIDAD = 'http://localhost:8082' // cuentas, sesiones y agentes
const CONFIGURACION = 'http://localhost:8084' // catálogo, configuración y auditoría

// Prefijo interno. No sale al backend: el proxy lo quita antes de reenviar.
const ESCRITURA = '/__escritura'

/**
 * Manda el `POST /v1/reservas` al núcleo y deja el resto en su sitio.
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
 * Es el ÚNICO caso que necesita este truco. Todo lo demás se reparte por
 * prefijo —`/v1/cuentas` y `/v1/sesiones` son de identidad, `/v1/config` y el
 * catálogo son de configuración— y esos prefijos ya distinguen el proceso sin
 * mirar el método.
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
        if (peticion.method === 'POST' && peticion.url?.startsWith('/v1/reservas')) {
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
      // Primero el más específico: la escritura de reservas, ya reescrita.
      [`${ESCRITURA}/v1`]: {
        target: NUCLEO,
        changeOrigin: true,
        rewrite: (ruta) => ruta.replace(ESCRITURA, ''),
      },

      // Identidad: todo lo que tiene que ver con quién es alguien. Vive aparte
      // porque manda correo, y un relé lento no puede arrastrar la reserva.
      '/v1/sesiones': { target: IDENTIDAD, changeOrigin: true },
      '/v1/cuentas': { target: IDENTIDAD, changeOrigin: true },
      '/v1/agentes': { target: IDENTIDAD, changeOrigin: true },

      // Configuración y Catálogo. Va contra el primario porque escribe, y por
      // eso no está con la lectura aunque `/v1/sedes` parezca una lectura más.
      '/v1/sedes': { target: CONFIGURACION, changeOrigin: true },
      '/v1/servicios': { target: CONFIGURACION, changeOrigin: true },
      '/v1/config': { target: CONFIGURACION, changeOrigin: true },
      '/v1/auditoria': { target: CONFIGURACION, changeOrigin: true },

      // Y el resto —reservas y disponibilidad— a la ruta de lectura.
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
