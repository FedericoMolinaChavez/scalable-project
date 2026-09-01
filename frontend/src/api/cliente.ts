import createClient from 'openapi-fetch'

import type { components, paths } from './esquema'

/**
 * Cliente HTTP tipado contra api/openapi.yaml.
 *
 * No hay tipos escritos a mano para las respuestas: `paths` sale de
 * `esquema.ts`, que se genera desde el contrato. Si el backend cambia una
 * respuesta y el contrato se regenera, esto deja de compilar. Ese es el punto:
 * la desincronización aparece al compilar y no en producción.
 *
 * La URL base es el propio origen, nunca una URL de backend embebida. En
 * desarrollo el proxy de Vite reenvía `/v1` al backend; en producción el
 * Gateway sirve ambos desde el mismo origen (ARQ-01). Así el cliente no se
 * comporta distinto en un sitio y en otro.
 *
 * Se resuelve a absoluta y no se deja en `/`: openapi-fetch construye un
 * objeto `Request`, y el `Request` de Node exige URL absoluta. Un navegador
 * resolvería la relativa contra el documento, pero en las pruebas —que corren
 * sobre Node— falla con "Failed to parse URL". El respaldo cubre el caso sin
 * `window`; en un SPA no ocurre, pero un `?.` de más cuesta menos que
 * depurarlo.
 */
const origen = globalThis.location?.origin ?? 'http://localhost'

export const cliente = createClient<paths>({
  baseUrl: origen,

  // La indirección no es gratuita en legibilidad, así que conviene el porqué:
  // openapi-fetch captura `globalThis.fetch` al crear el cliente, y este
  // cliente es un singleton de módulo que se construye al importarlo. Pasando
  // la referencia directa, cualquier sustitución posterior del global —lo que
  // hace una prueba— llegaría tarde y las peticiones se irían a la red de
  // verdad. Resolviéndolo en cada llamada, el cliente usa siempre el `fetch`
  // vigente. En producción es una llamada indirecta y nada más.
  //
  // Si el simulado de peticiones crece, la alternativa es MSW, que intercepta
  // a nivel de red y no obliga a esta indirección.
  fetch: (peticion) => globalThis.fetch(peticion),
})

/**
 * Tenant de desarrollo. Provisional, igual que la cabecera que lo transporta:
 * cuando exista autenticación (RF-12) saldrá del token y esto desaparece.
 *
 * Vive aquí y no en `consultas.ts` porque lo mandan también las escrituras y la
 * identificación. Dejarlo en el módulo de lecturas obligaba a que `sesion.ts` lo
 * importara de allí, y `consultas.ts` a su vez importara de `sesion.ts` para la
 * autorización: un ciclo entre dos módulos que no se necesitan entre sí.
 */
const TENANT = '11111111-1111-1111-1111-111111111111'

export const cabeceras = { 'X-Tenant-Id': TENANT }

/** Atajos a los esquemas del contrato, para no repetir `components['schemas']`. */
export type Esquemas = components['schemas']

export type Sede = Esquemas['Sede']
export type Servicio = Esquemas['Servicio']
export type Reserva = Esquemas['Reserva']
export type NuevaReserva = Esquemas['NuevaReserva']
export type Disponibilidad = Esquemas['Disponibilidad']
export type Franja = Esquemas['Franja']
export type EstadoReserva = Esquemas['EstadoReserva']
export type Problema = Esquemas['Problema']

/**
 * Error de la API con el cuerpo RFC 9457 ya tipado.
 *
 * Se conserva `status` aparte del problema porque una respuesta de error puede
 * no traer cuerpo (un 502 del Gateway, por ejemplo) y la interfaz igualmente
 * tiene que poder distinguir un 409 de un 500.
 */
export class ErrorApi extends Error {
  readonly status: number
  readonly problema?: Problema

  constructor(status: number, problema?: Problema) {
    super(problema?.title ?? `La petición falló con estado ${status}`)
    this.name = 'ErrorApi'
    this.status = status
    this.problema = problema
  }

  /**
   * El horario ya estaba tomado (RF-01, flujo alternativo 3).
   *
   * No es un fallo: es el resultado normal de perder una carrera por el mismo
   * cupo. La interfaz debe ofrecer otra franja, nunca reintentar la misma.
   */
  get esHorarioOcupado(): boolean {
    return this.status === 409
  }
}
