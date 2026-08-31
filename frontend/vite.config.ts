import tailwind from '@tailwindcss/vite'
import react from '@vitejs/plugin-react'
// De vitest/config y no de vite: el defineConfig de Vite no conoce la clave
// `test` y TypeScript la rechazaría.
import { defineConfig } from 'vitest/config'

export default defineConfig({
  plugins: [react(), tailwind()],

  server: {
    port: 5173,
    proxy: {
      // El frontend llama a /v1/... como si fuera del mismo origen y Vite lo
      // reenvía al backend. Así no hace falta configurar CORS en desarrollo ni
      // meter la URL del backend en el código: en producción el Gateway sirve
      // ambos desde el mismo origen (ARQ-01), y el cliente HTTP no tiene que
      // comportarse distinto en un sitio y en otro.
      '/v1': {
        target: 'http://localhost:8080',
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
