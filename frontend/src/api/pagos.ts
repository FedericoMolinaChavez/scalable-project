import { queryOptions } from '@tanstack/react-query'

import { cabeceras, cliente, ErrorApi, type Esquemas } from './cliente'
import { autorizacion } from './sesion'

/**
 * El pago (RF-01, RF-33) y el comprobante (RF-34).
 *
 * Lo que este módulo NO hace es tan importante como lo que hace: no toca la
 * tarjeta. El número lo recoge un iframe de Stripe y viaja directamente a
 * Stripe; de aquí solo sale el `client_secret`, que autoriza a confirmar ESE
 * cobro y nada más. Es lo que hace que RNF-05 sea cierto y no una promesa.
 */

export type IntencionPago = Esquemas['IntencionPago']
export type EstadoConfirmacion = Esquemas['EstadoConfirmacion']
export type Comprobante = Esquemas['Comprobante']

/**
 * Abre —o recupera— el cobro de una reserva pendiente.
 *
 * Es idempotente en el servidor: llamarla dos veces devuelve la misma
 * intención, porque el motor solo admite un pago vivo por reserva. Eso importa
 * aquí porque React en modo estricto monta los efectos dos veces en desarrollo,
 * y sin esa garantía cada recarga de la página de pago abriría un cobro nuevo.
 */
export async function crearIntencion(reservaId: string): Promise<IntencionPago> {
  const { data, error, response } = await cliente.POST('/v1/pagos/intencion', {
    params: { header: cabeceras },
    body: { reserva_id: reservaId },
  })

  if (error) throw new ErrorApi(response.status, error)
  return data
}

/**
 * Dónde está la confirmación asíncrona (RF-33).
 *
 * Existe por la asimetría del flujo: cuando Stripe.js dice «succeeded» en el
 * navegador, la reserva de este sistema todavía está `pendiente`. Quien la
 * confirma es el webhook, y eso pasa unos cientos de milisegundos más tarde,
 * fuera del presupuesto de latencia del núcleo. Sin esta consulta, la interfaz
 * no tendría forma de saber cuándo dejar de esperar.
 */
export const claveConfirmacion = (reservaId: string) => ['confirmacion', reservaId] as const

export const consultaConfirmacion = (reservaId: string, esperando: boolean) =>
  queryOptions({
    queryKey: claveConfirmacion(reservaId),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/pagos/{reserva_id}/estado', {
        params: { header: cabeceras, path: { reserva_id: reservaId } },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data
    },

    /**
     * Se consulta cada segundo, y solo mientras haga falta.
     *
     * `esperando` lo decide quien llama: en cuanto la reserva deja de estar
     * pendiente no hay nada más que esperar y el sondeo se apaga solo. Un
     * intervalo fijo seguiría preguntando para siempre por una reserva ya
     * confirmada, multiplicado por cada pestaña abierta.
     *
     * Un segundo y no menos: el webhook tarda lo que tarde Stripe en emitirlo,
     * y preguntar diez veces por segundo no lo adelanta.
     */
    refetchInterval: esperando ? 1_000 : false,
    enabled: esperando,

    /**
     * Sin `staleTime`. Es lo contrario de la disponibilidad, que se cachea los
     * 2 s que RNF-10 autoriza: aquí lo que se pregunta es justamente si algo
     * acaba de cambiar, así que una respuesta cacheada no responde nada.
     */
    staleTime: 0,
  })

/**
 * Cuánto se espera al webhook antes de dejar de sondear.
 *
 * Pasado este tiempo la interfaz deja de preguntar y lo dice, pero NO trata el
 * pago como perdido: el conciliador de RF-33 acaba preguntándole a Stripe por
 * su cuenta, así que insistir desde aquí no acelera nada y el cobro no se cae.
 * Lo que cambia es el mensaje: de «confirmando» a «se está tardando más de lo
 * normal», que es información y no una alarma.
 */
export const ESPERA_MAXIMA_MS = 30_000

/** El comprobante de una reserva pagada (RF-34). */
export const claveComprobante = (reservaId: string) => ['comprobante', reservaId] as const

export const consultaComprobante = (reservaId: string, habilitada: boolean) =>
  queryOptions({
    queryKey: claveComprobante(reservaId),
    queryFn: async () => {
      const { data, error, response } = await cliente.GET('/v1/reservas/{id}/comprobante', {
        params: { header: { ...cabeceras, ...autorizacion() }, path: { id: reservaId } },
      })
      if (error) throw new ErrorApi(response.status, error)
      return data
    },

    enabled: habilitada,

    /**
     * Un 404 aquí no siempre es definitivo: el comprobante lo emite un
     * trabajador unos segundos después de que el pago se confirme, así que
     * puede no existir todavía. Se reintenta unas pocas veces y se abandona.
     *
     * Es la única excepción a la regla de «ningún 4xx se reintenta», y se
     * sostiene porque aquí el 404 no significa «no existe» sino «todavía no».
     */
    retry: (intentos, error) =>
      error instanceof ErrorApi && error.status === 404 && intentos < 5,
    retryDelay: 2_000,

    /**
     * El enlace del comprobante es firmado y caduca en minutos. Cachearlo más
     * tiempo que su vigencia daría una URL que el navegador acepta y el almacén
     * rechaza.
     */
    staleTime: 60_000,
  })
