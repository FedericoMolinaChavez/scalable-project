import { Elements, PaymentElement, useElements, useStripe } from '@stripe/react-stripe-js'
import { loadStripe, type Stripe } from '@stripe/stripe-js'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useMemo, useState } from 'react'

import { ErrorApi, type Reserva } from '../api/cliente'
import { claves } from '../api/consultas'
import {
  consultaConfirmacion,
  crearIntencion,
  ESPERA_MAXIMA_MS,
  type IntencionPago,
} from '../api/pagos'
import { MensajeError } from './EstadoConsulta'

/**
 * El paso de pago (RF-01) y la espera de la confirmación asíncrona (RF-33).
 *
 * Tres cosas de este componente no son decisiones de diseño y no deberían
 * cambiar aunque cambie todo lo demás:
 *
 *   1. **El reloj no se va.** El bloqueo ES la reserva pendiente (RF-27), así
 *      que cuánto queda es lo que decide si el cupo sigue siendo suyo. Por eso
 *      se cobra dentro de la página y no en una redirección a Stripe: una
 *      pasarela alojada se lleva a la persona justo durante la parte más
 *      urgente del flujo y devuelve un contexto que ya no tiene el reloj.
 *
 *   2. **«Succeeded» de Stripe no es «confirmada» de este sistema.** Cuando
 *      Stripe.js dice que el cobro salió, la reserva sigue `pendiente` hasta
 *      que llegue el webhook. Enseñar «listo» en ese instante sería mentir
 *      durante unos cientos de milisegundos, y en el peor caso durante mucho
 *      más si el webhook se retrasa.
 *
 *   3. **Una tarjeta rechazada no cierra nada.** El intento sigue vivo y se
 *      puede reintentar con otra, que es literalmente el bucle que RF-01
 *      dibuja. Lo que la interfaz tiene que hacer es decir POR QUÉ falló y
 *      dejar volver a intentarlo, no volver al principio.
 */

/**
 * Stripe.js se carga una vez por clave, no una vez por render.
 *
 * `loadStripe` inyecta un script en la página; llamarlo en cada render lo
 * inyectaría otra vez y las instancias dejarían de compartir estado. El caché
 * va por clave publicable porque esa clave llega de la API —es del entorno, no
 * del artefacto— y podría cambiar sin recargar la aplicación.
 */
const cargados = new Map<string, Promise<Stripe | null>>()

function stripeDe(clavePublicable: string): Promise<Stripe | null> {
  let promesa = cargados.get(clavePublicable)
  if (promesa === undefined) {
    promesa = loadStripe(clavePublicable)
    cargados.set(clavePublicable, promesa)
  }
  return promesa
}

export function Pago({
  reserva,
  onConfirmada,
}: {
  reserva: Reserva
  onConfirmada: () => void
}) {
  // La intención se abre UNA vez por reserva, y las cinco opciones de abajo
  // existen para que eso sea cierto por construcción y no por suerte.
  //
  // Abrir un cobro es una escritura contra un tercero. El servidor la hace
  // idempotente —el motor solo admite un pago vivo por reserva— pero apoyarse
  // en eso desde el cliente convierte una garantía en una excusa para llamar de
  // más. Y con la configuración por defecto se llama de más: una consulta que
  // falló está SIEMPRE obsoleta, así que `refetchOnWindowFocus` la vuelve a
  // lanzar cada vez que alguien cambia de pestaña y vuelve. Con Stripe caído,
  // eso es una tormenta de reintentos contra el proveedor exactamente cuando
  // menos lo aguanta.
  //
  // La única forma de volver a intentarlo es el botón de reintentar, que es
  // una persona decidiéndolo.
  const intencion = useQuery({
    queryKey: ['intencion', reserva.id],
    queryFn: () => crearIntencion(reserva.id),

    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
    refetchOnMount: false,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  if (intencion.isPending) {
    return (
      <p aria-busy="true" role="status" className="etiqueta text-[var(--color-marina-media)]">
        Preparando el pago…
      </p>
    )
  }

  if (intencion.isError) {
    return <NoSePudoAbrir error={intencion.error} onReintentar={() => void intencion.refetch()} />
  }

  return (
    <Cobro intencion={intencion.data} reserva={reserva} onConfirmada={onConfirmada} />
  )
}

/**
 * El fallo al abrir el cobro se explica según de quién sea.
 *
 * Un 502 es de Stripe y la reserva sigue apartada: reintentar tiene sentido. Un
 * 409 es que la reserva ya no admite pago —venció, o ya está confirmada— y
 * reintentar no puede cambiar eso.
 */
function NoSePudoAbrir({ error, onReintentar }: { error: unknown; onReintentar: () => void }) {
  const esApi = error instanceof ErrorApi

  if (esApi && error.status === 409) {
    return (
      <div
        role="alert"
        className="border border-[var(--color-rojo)] bg-[var(--color-cupon)] p-4"
      >
        <span className="sello estampado">Vencida</span>
        <p className="titular mt-3 text-[1.0625rem]">Este horario ya no se puede pagar</p>
        <p className="mt-2 max-w-[52ch] text-[0.875rem] text-[var(--color-marina-media)]">
          {error.problema?.detail ?? 'El bloqueo venció o la reserva cambió de estado.'} No se te
          ha cobrado nada.
        </p>
      </div>
    )
  }

  return <MensajeError error={error} onReintentar={onReintentar} />
}

function Cobro({
  intencion,
  reserva,
  onConfirmada,
}: {
  intencion: IntencionPago
  reserva: Reserva
  onConfirmada: () => void
}) {
  const stripe = useMemo(() => stripeDe(intencion.clave_publicable), [intencion.clave_publicable])

  return (
    <Elements
      stripe={stripe}
      options={{
        clientSecret: intencion.client_secret,
        locale: 'es',

        // La apariencia se hereda de los tokens de la aplicación, no se
        // reinventa aquí: el formulario de Stripe vive en un iframe y no puede
        // leer nuestro CSS, así que los valores se le pasan explícitamente.
        // Sin esto, el único elemento con otra tipografía y otro radio de borde
        // es justo el que pide la tarjeta, que es el peor sitio para que algo
        // parezca ajeno a la página.
        appearance: apariencia(),
      }}
    >
      <Formulario reserva={reserva} monto={intencion.monto} onConfirmada={onConfirmada} />
    </Elements>
  )
}

function Formulario({
  reserva,
  monto,
  onConfirmada,
}: {
  reserva: Reserva
  monto: IntencionPago['monto']
  onConfirmada: () => void
}) {
  const stripe = useStripe()
  const elements = useElements()
  const clienteConsultas = useQueryClient()

  // `cobrado` marca el momento en que Stripe dijo que sí, que NO es el momento
  // en que la reserva queda confirmada. A partir de aquí la interfaz espera al
  // webhook (RF-33) en vez de afirmar nada.
  const [cobrado, setCobrado] = useState(false)

  const pagar = useMutation({
    mutationFn: async () => {
      if (!stripe || !elements) {
        throw new Error('El formulario de pago todavía no está listo.')
      }

      const { error, paymentIntent } = await stripe.confirmPayment({
        elements,

        // Sin redirección. El PaymentIntent se creó con `allow_redirects:
        // never`, así que ningún método de pago se va a llevar a la persona
        // fuera; esto se lo dice también a Stripe.js para que no monte el
        // camino de vuelta.
        redirect: 'if_required',
      })

      if (error) {
        // El mensaje de Stripe se enseña tal cual. Reescribirlo produciría dos
        // vocabularios para el mismo rechazo, y el suyo ya está traducido y
        // dice cosas accionables ("tu tarjeta no tiene fondos suficientes")
        // que este código no puede saber.
        throw new Error(error.message ?? 'No se pudo completar el pago.')
      }

      return paymentIntent
    },

    onSuccess: () => {
      setCobrado(true)
      // La disponibilidad y el listado cambian en cuanto la reserva se
      // confirme, así que se invalidan ya: la confirmación llega por otro
      // camino y nadie va a volver a pedirlas por su cuenta.
      void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
    },
  })

  if (cobrado) {
    return <Esperando reserva={reserva} onConfirmada={onConfirmada} />
  }

  return (
    <form
      onSubmit={(evento) => {
        evento.preventDefault()
        pagar.mutate()
      }}
    >
      <PaymentElement />

      <button type="submit" className="boton mt-5 w-full" disabled={!stripe || pagar.isPending}>
        {pagar.isPending ? 'Procesando…' : `Pagar ${monto.monto} ${monto.moneda}`}
      </button>

      {pagar.isError ? (
        <div
          role="alert"
          className="mt-4 border border-[var(--color-rojo)] bg-[var(--color-cupon)]"
        >
          <div className="tira h-1" aria-hidden="true" />
          <div className="px-3 py-2.5">
            <p className="text-[0.875rem] font-medium text-[var(--color-rojo)]">
              {pagar.error.message}
            </p>
          {/* No se vuelve al principio: el intento sigue vivo y admite otra
              tarjeta. Es el bucle de reintento que RF-01 describe. */}
            <p className="mt-1 text-[0.8125rem] text-[var(--color-marina-media)]">
              Tu horario sigue apartado. Puedes intentarlo con otro método de pago.
            </p>
          </div>
        </div>
      ) : null}
    </form>
  )
}

/**
 * La espera del webhook (RF-33).
 *
 * Se consulta el estado hasta que la reserva deje de estar pendiente, con un
 * límite. Pasado ese límite deja de preguntar y lo dice, pero no declara el
 * pago perdido: el conciliador de RF-33 acaba preguntándole a Stripe por su
 * cuenta, así que el cobro no se cae por dejar de mirar.
 */
function Esperando({ reserva, onConfirmada }: { reserva: Reserva; onConfirmada: () => void }) {
  const [agotada, setAgotada] = useState(false)

  const confirmacion = useQuery(consultaConfirmacion(reserva.id, !agotada))

  useEffect(() => {
    const id = setTimeout(() => setAgotada(true), ESPERA_MAXIMA_MS)
    return () => clearTimeout(id)
  }, [])

  const estado = confirmacion.data?.reserva_estado

  useEffect(() => {
    if (estado === 'confirmada') onConfirmada()
  }, [estado, onConfirmada])

  if (estado === 'confirmada') {
    return (
      <div role="status" className="talon p-4">
        <span className="etiqueta text-[var(--color-carbon)]">Copia carbón</span>
        <p className="titular mt-1.5 text-[1.0625rem] text-[var(--color-carbon)]">
          Pago confirmado
        </p>
      </div>
    )
  }

  if (agotada) {
    return (
      <div role="status" className="border border-[var(--color-carbon)] bg-[var(--color-cupon)] p-4">
        <span className="etiqueta text-[var(--color-carbon)]">Cobro hecho</span>
        <p className="titular mt-1.5 text-[1.0625rem]">La confirmación está tardando</p>
        <p className="mt-2 max-w-[52ch] text-[0.875rem] text-[var(--color-marina-media)]">
          El cobro se hizo. No hace falta pagar otra vez ni volver a reservar: el sistema
          comprueba el estado con el proveedor y lo resuelve solo. Te llegará un correo en
          cuanto quede.
        </p>
      </div>
    )
  }

  return (
    <div
      role="status"
      aria-busy="true"
      className="border border-[var(--color-carbon)] bg-[var(--color-cupon)] p-4"
    >
      <span className="etiqueta text-[var(--color-carbon)]">Confirmando</span>
      {/* Se dice que la espera es normal, porque lo es: la confirmación llega
          por webhook y no en la misma respuesta. Sin decirlo, un par de
          segundos de reloj parecen un fallo. */}
      <p className="mt-2 max-w-[52ch] text-[0.875rem] text-[var(--color-marina-media)]">
        Tu pago salió bien y estamos sellando la reserva. Tarda un momento; no cierres la
        página.
      </p>

      <div className="mt-3 h-px overflow-hidden bg-[var(--color-filete)]" aria-hidden="true">
        <div
          className="h-px bg-[var(--color-carbon)]"
          style={{ animation: 'avanzar 1.4s var(--salida) infinite' }}
        />
      </div>
    </div>
  )
}

/**
 * La apariencia que se le pasa al iframe de Stripe.
 *
 * Los valores se leen de las variables CSS de la aplicación en tiempo de
 * ejecución en lugar de escribirse aquí: así el formulario de pago sigue al
 * tema de la página —incluido el modo oscuro— sin mantener dos paletas.
 */
function apariencia() {
  const raiz = getComputedStyle(document.documentElement)
  const token = (nombre: string, respaldo: string) =>
    raiz.getPropertyValue(nombre).trim() || respaldo

  return {
    theme: 'stripe' as const,
    variables: {
      colorPrimary: token('--color-acento', '#4338ca'),
      colorBackground: token('--color-superficie', '#ffffff'),
      colorText: token('--color-texto', '#18181b'),
      colorDanger: token('--color-peligro', '#b91c1c'),
      fontFamily: token('--fuente-cuerpo', 'system-ui, sans-serif'),
      borderRadius: token('--radio-md', '8px'),
      spacingUnit: '4px',
    },
  }
}
