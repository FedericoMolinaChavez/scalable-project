import { cabeceras, cliente, ErrorApi, type NuevaReserva, type Reserva } from './cliente'
import { autorizacion } from './sesion'

/**
 * Crear una reserva: la única escritura de esta rebanada.
 *
 * Vive aparte de `consultas.ts` por la misma razón por la que el backend
 * separa el núcleo del servicio de consulta (ARQ-01): la escritura tiene
 * garantías, reintentos y modos de fallo propios, y mezclarla con las lecturas
 * invita a tratarla igual que ellas.
 */
export async function crearReserva(
  nueva: NuevaReserva,
  claveIdempotencia: string,
): Promise<Reserva> {
  const { data, error, response } = await cliente.POST('/v1/reservas', {
    params: {
      header: {
        ...cabeceras,

        // Obligatoria, no opcional. Sin ella, un reintento tras un timeout
        // crearía un segundo bloqueo sobre otro horario que nadie libera hasta
        // que venza su TTL: se deniega inventario propio por accidente.
        // Repetir la misma clave devuelve la reserva ya creada.
        'Idempotency-Key': claveIdempotencia,
      },
    },
    body: nueva,
  })

  if (error) throw new ErrorApi(response.status, error)
  return data.reserva
}

/**
 * Cancelar una reserva (RF-06).
 *
 * Necesita el token: cancelar la reserva de otra persona es exactamente lo que
 * la identificación existe para impedir. Una reserva ajena responde 404, igual
 * que una inexistente, así que la interfaz no puede —ni debe— distinguirlas.
 */
export async function cancelarReserva(id: string): Promise<Reserva> {
  const { data, error, response } = await cliente.POST('/v1/reservas/{id}/cancelacion', {
    params: { header: { ...cabeceras, ...autorizacion() }, path: { id } },
  })

  if (error) throw new ErrorApi(response.status, error)
  return data
}

/**
 * Clave de idempotencia por franja, estable mientras la franja lo sea.
 *
 * Es la parte que no se puede resolver con un `crypto.randomUUID()` en el
 * momento de enviar. La clave tiene que ser la MISMA cuando alguien reintenta
 * el mismo horario —si no, el reintento crea un segundo bloqueo— y distinta
 * cuando elige otro. Atarla a la identidad de la franja da exactamente eso, y
 * sin guardar nada entre recargas: recargar la página es empezar de cero, que
 * es lo que la persona espera.
 */
const clavesPorFranja = new Map<string, string>()

export function claveDeFranja(recursoId: string, inicio: string): string {
  const franja = `${recursoId}@${inicio}`

  let clave = clavesPorFranja.get(franja)
  if (clave === undefined) {
    clave = crypto.randomUUID()
    clavesPorFranja.set(franja, clave)
  }

  return clave
}

/**
 * Olvida la clave de una franja tras un 409.
 *
 * Sin esto, quien pierde la carrera y vuelve a intentar esa misma franja más
 * tarde —cuando el otro bloqueo ha vencido y el cupo está libre otra vez—
 * reutilizaría una clave que ya no corresponde a ninguna reserva creada. No
 * rompería nada hoy, pero ata una clave a un intento fallido para siempre, y
 * las claves de idempotencia solo son útiles mientras signifiquen algo.
 */
export function olvidarClave(recursoId: string, inicio: string): void {
  clavesPorFranja.delete(`${recursoId}@${inicio}`)
}
