import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ErrorApi, type Reserva } from '../api/cliente'
import { claves, consultaReservas } from '../api/consultas'
import { cancelarReserva } from '../api/mutaciones'
import { consultaComprobante } from '../api/pagos'
import { canjearCodigo, olvidarSesion, sesionGuardada, solicitarCodigo } from '../api/sesion'
import { Campo, Campos, Cupon, Estado, Pestana, Pestanas } from '../componentes/cupon'
import { EstadoConsulta, MensajeError, Vacio } from '../componentes/EstadoConsulta'

/**
 * Mis reservas (RF-02), cancelación (RF-06) y comprobante (RF-34).
 *
 * Dos pantallas en una, porque son dos estados del mismo sitio: sin
 * identificarse se pide un código; con el token, se ve la cartera de cupones.
 *
 * Lo que queda fijado:
 *
 *   - La pantalla NUNCA dice si un correo existe. Tras pedir el código responde
 *     lo mismo en todos los casos (RF-12 A12), porque una respuesta que los
 *     distinga convierte esto en un buscador de clientes del negocio.
 *   - Un 401 no se trata como un error más: significa que la identificación
 *     caducó, y la salida es volver a pedir un código, no reintentar.
 *   - Nada desaparece. Una reserva cancelada o vencida sigue en la cartera con
 *     su sello, porque el historial de RF-28 es append-only y la interfaz no
 *     puede contar otra cosa.
 */
export function MisReservas() {
  const [sesion, setSesion] = useState(() => sesionGuardada())

  if (!sesion) {
    return <Identificarse onIdentificado={() => setSesion(sesionGuardada())} />
  }

  return (
    <Reservas
      onCaducada={() => {
        olvidarSesion()
        setSesion(null)
      }}
    />
  )
}

/**
 * La puerta: un código de un solo uso al correo con el que se reservó.
 *
 * Es el mostrador de la compañía: se presenta el correo, llega un código, y con
 * él se abre la cartera. No hay contraseña porque no hay cuenta — quien reservó
 * como invitado solo puede demostrar que controla ese correo.
 */
function Identificarse({ onIdentificado }: { onIdentificado: () => void }) {
  const [destino, setDestino] = useState('')
  const [codigo, setCodigo] = useState('')
  const [enviado, setEnviado] = useState(false)

  const pedir = useMutation({
    mutationFn: () => solicitarCodigo(destino),
    onSuccess: () => setEnviado(true),
  })

  const canjear = useMutation({
    mutationFn: () => canjearCodigo(destino, codigo),
    onSuccess: onIdentificado,
  })

  return (
    <section>
      <h1 className="titular mt-3 max-w-[16ch] text-[clamp(2.25rem,7vw,3.75rem)]">
        Abre tu cartera
      </h1>

      <p className="mt-4 max-w-[56ch] text-[0.9375rem] text-[var(--color-marina-media)]">
        No hace falta cuenta. Te mandamos un código de seis dígitos al correo con el que
        reservaste, y con él ves y gestionas lo que es tuyo.
      </p>

      <div className="mt-10 max-w-md">
        <Pestanas>
          <Pestana>Identificación</Pestana>
        </Pestanas>

        <Cupon>
          <form
            className="p-4"
            onSubmit={(evento) => {
              evento.preventDefault()
              if (enviado) canjear.mutate()
              else pedir.mutate()
            }}
          >
            <label htmlFor="destino" className="etiqueta">
              Correo
            </label>
            <input
              id="destino"
              type="email"
              value={destino}
              onChange={(evento) => setDestino(evento.target.value)}
              // Cambiar el correo después de pedir el código vuelve al
              // principio: el código que llegó es del correo anterior.
              readOnly={enviado}
              className="entrada mt-1.5 read-only:bg-[var(--color-papel)] read-only:text-[var(--color-marina-media)]"
              autoComplete="email"
              required
            />

            {enviado ? (
              <>
                {/* El mensaje no afirma que el correo exista: dice qué pasa SI
                    existe. Es la misma frase para una dirección con reservas y
                    para una inventada. */}
                <p
                  role="status"
                  className="mt-4 border border-[var(--color-copia)] bg-[var(--color-copia-papel)] px-3 py-2 text-[0.8125rem] text-[var(--color-carbon)]"
                >
                  Si hay reservas hechas con ese correo, el código ya va en camino. Caduca en
                  unos minutos.
                </p>

                <label htmlFor="codigo" className="etiqueta mt-4 block">
                  Código
                </label>
                <input
                  id="codigo"
                  inputMode="numeric"
                  pattern="[0-9]{6}"
                  maxLength={6}
                  value={codigo}
                  onChange={(evento) => setCodigo(evento.target.value.replace(/\D/g, ''))}
                  className="entrada mt-1.5 text-[1.25rem] tracking-[0.4em]"
                  autoComplete="one-time-code"
                  required
                />
              </>
            ) : null}

            <div className="mt-5 flex flex-wrap items-center gap-x-4 gap-y-2">
              <button type="submit" className="boton" disabled={pedir.isPending || canjear.isPending}>
                {enviado ? 'Entrar' : 'Enviarme el código'}
              </button>

              {enviado ? (
                <button
                  type="button"
                  className="boton boton--texto"
                  onClick={() => {
                    setEnviado(false)
                    setCodigo('')
                    canjear.reset()
                  }}
                >
                  Usar otro correo
                </button>
              ) : null}
            </div>
          </form>
        </Cupon>

        {pedir.isError ? (
          <div className="mt-6">
            <MensajeError error={pedir.error} />
          </div>
        ) : null}
        {canjear.isError ? (
          <div className="mt-6">
            <MensajeError error={canjear.error} />
          </div>
        ) : null}
      </div>
    </section>
  )
}

/** La cartera: un cupón por reserva, en orden. */
function Reservas({ onCaducada }: { onCaducada: () => void }) {
  const clienteConsultas = useQueryClient()
  const consulta = useQuery(consultaReservas())

  // El token caducó mientras la pestaña estaba abierta. No es un error que
  // mostrar, es una identificación que hay que rehacer.
  if (consulta.isError && consulta.error instanceof ErrorApi && consulta.error.status === 401) {
    onCaducada()
    return null
  }

  return (
    <section>
      <div className="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-2">
        <button
          type="button"
          className="boton boton--texto"
          onClick={() => {
            olvidarSesion()
            void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
            onCaducada()
          }}
        >
          Salir
        </button>
      </div>

      <h1 className="titular mt-3 max-w-[16ch] text-[clamp(2.25rem,7vw,3.75rem)]">
        Tu cartera
      </h1>

      <div className="mt-10">
        <EstadoConsulta
          consulta={consulta}
          vacio={
            <Vacio titulo="No hay reservas hechas con ese correo">
              Si reservaste con otra dirección, sal y vuelve a identificarte con esa.
            </Vacio>
          }
        >
          {(reservas) => (
            <ul className="grid gap-6 sm:grid-cols-2">
              {reservas.map((reserva) => (
                <li key={reserva.id}>
                  <Talon reserva={reserva} onCaducada={onCaducada} />
                </li>
              ))}
            </ul>
          )}
        </EstadoConsulta>
      </div>
    </section>
  )
}

/**
 * Un cupón de la cartera.
 *
 * Lleva SIEMPRE el mismo bloque: tramo, estado, precio y referencia. Que sea el
 * mismo en todas las pantallas es lo que hace que una reserva sea reconocible
 * como el mismo objeto en «reservar» y aquí.
 */
function Talon({ reserva, onCaducada }: { reserva: Reserva; onCaducada: () => void }) {
  const clienteConsultas = useQueryClient()

  const cancelar = useMutation({
    mutationFn: () => cancelarReserva(reserva.id),
    onSuccess: () => {
      void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
    },
    onError: (error) => {
      if (error instanceof ErrorApi && error.status === 401) onCaducada()
    },
  })

  // Solo se ofrece cancelar donde el backend lo admite (RF-28). Un botón que
  // siempre falla es peor que no tenerlo: enseña una salida que no existe.
  const cancelable = reserva.estado === 'pendiente' || reserva.estado === 'confirmada'

  // La fila de acciones solo existe si hay alguna. Sin esta comprobación, un
  // cupón anulado dibuja igualmente su filete superior y su relleno, y queda
  // una franja vacía al pie que se lee como algo que no cargó.
  const hayAcciones = cancelable || puedeTenerComprobante(reserva.estado)

  return (
    <div>
      <Pestanas>
        <Pestana>Reserva</Pestana>
      </Pestanas>

      <Cupon className="flex">
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2 p-4">
            <span className="cifra text-[1.0625rem] leading-tight tracking-[-0.02em]">
              {formatearPeriodo(reserva.periodo)}
            </span>
            <Estado estado={reserva.estado} />
          </div>

          <Campos columnas="1fr 1fr">
            <Campo etiqueta="Precio" cifra>
              {reserva.precio_cobrado.monto} {reserva.precio_cobrado.moneda}
            </Campo>
            <Campo etiqueta="Referencia" cifra>
              <span className="text-[0.75rem] break-all">{reserva.id.slice(0, 13)}</span>
            </Campo>
          </Campos>

          {hayAcciones ? (
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 border-t border-[var(--color-filete)] p-3">
              {cancelable ? (
                <button
                  type="button"
                  className="boton boton--secundario"
                  disabled={cancelar.isPending}
                  onClick={() => cancelar.mutate()}
                >
                  {cancelar.isPending ? 'Cancelando…' : 'Cancelar'}
                </button>
              ) : null}

              <Comprobante reservaId={reserva.id} estado={reserva.estado} />
            </div>
          ) : null}
        </div>

        {/* La tira solo cuelga de lo provisional. */}
        {reserva.estado === 'pendiente' ? (
          <div className="tira w-2.5 shrink-0" aria-hidden="true" />
        ) : null}
      </Cupon>

      {/* El motivo del rechazo va junto a la reserva que lo provocó, no en una
          alerta global: con varias reservas en pantalla, un mensaje suelto no
          dice a cuál se refiere. */}
      {cancelar.isError ? (
        <div className="mt-3">
          <MensajeError error={cancelar.error} />
        </div>
      ) : null}
    </div>
  )
}

/**
 * El comprobante de pago (RF-34), en el papel de la copia carbón.
 *
 * Tres estados y ninguno es un error:
 *
 *   - la reserva no llegó a cobrarse, y entonces no hay nada que enseñar;
 *   - se cobró pero el documento todavía se está generando, porque lo emite un
 *     trabajador unos segundos después de la confirmación;
 *   - está listo, y el enlace es firmado y de vida corta.
 *
 * El enlace se abre en una pestaña nueva y con `rel="noreferrer"`: lleva una
 * firma en la URL, y el `Referer` la filtraría al sitio de destino si alguna vez
 * dejara de ser el propio almacén.
 */
/**
 * Los estados que implican un cobro confirmado, y por tanto un comprobante.
 *
 * Una pendiente todavía no pagó y una expirada nunca lo hizo, así que pedirles
 * el comprobante garantiza un 404 en cada render.
 *
 * Vive fuera del componente porque lo consulta también quien decide si hay
 * fila de acciones que dibujar: si esa decisión se tomara con otra regla, un
 * cupón podría dibujar la fila vacía o esconder un comprobante que sí existe.
 */
function puedeTenerComprobante(estado: Reserva['estado']): boolean {
  return (
    estado === 'confirmada' ||
    estado === 'en_curso' ||
    estado === 'completada' ||
    estado === 'no_show'
  )
}

function Comprobante({ reservaId, estado }: { reservaId: string; estado: Reserva['estado'] }) {
  const puedeTenerlo = puedeTenerComprobante(estado)

  const comprobante = useQuery(consultaComprobante(reservaId, puedeTenerlo))

  if (!puedeTenerlo || comprobante.isError) return null

  if (comprobante.isPending) {
    return (
      <span className="etiqueta text-[var(--color-marina-tenue)]">Comprobante en camino</span>
    )
  }

  if (!comprobante.data.url) {
    // La fila existe y el documento aún no: el número ya es información útil, y
    // decir «no hay comprobante» sería falso.
    return (
      <span className="talon inline-flex items-center gap-2 px-2 py-1">
        <span className="etiqueta text-[var(--color-carbon)]">Comprobante</span>
        <span className="cifra text-[0.75rem]">{comprobante.data.numero}</span>
      </span>
    )
  }

  return (
    <a
      href={comprobante.data.url}
      target="_blank"
      rel="noreferrer"
      className="talon inline-flex items-center gap-2 px-2 py-1 no-underline transition-colors hover:bg-[var(--color-copia)]"
    >
      <span className="etiqueta text-[var(--color-carbon)]">Comprobante</span>
      <span className="cifra text-[0.75rem] underline decoration-1 underline-offset-2">
        {comprobante.data.numero}
      </span>
    </a>
  )
}

/**
 * El período es semiabierto `[inicio, fin)`, así que el instante final NO
 * pertenece a la reserva. Se muestra igualmente porque para una persona
 * "10:00–11:00" es lo natural, pero ningún cálculo debe tratar `fin` como
 * ocupado.
 *
 * Aquí se usa la zona del navegador, y es una deuda consciente: esta lista
 * mezcla reservas de varias sedes y cada una tiene la suya (RF-38), así que
 * formatear bien exige saber a qué sede pertenece cada fila. El contrato
 * todavía no trae la sede en la reserva. Ver src/formato/zona.ts, que sí lo
 * hace bien donde la sede se conoce.
 */
function formatearPeriodo(periodo: { inicio: string; fin: string }): string {
  const inicio = new Date(periodo.inicio)
  const fin = new Date(periodo.fin)

  const completo = new Intl.DateTimeFormat('es', { dateStyle: 'medium', timeStyle: 'short' })
  const soloHora = new Intl.DateTimeFormat('es', { timeStyle: 'short' })

  // La fecha se escribe UNA vez cuando el tramo no cruza la medianoche, que es
  // el caso de casi toda cita. Repetirla en los dos extremos —«1 sept 2026,
  // 15:00 – 1 sept 2026, 16:00»— obliga a leer catorce caracteres para
  // descubrir que dicen lo mismo, y en una lista de cinco cupones eso es lo
  // único que se ve.
  const mismoDia = inicio.toDateString() === fin.toDateString()

  return mismoDia
    ? `${completo.format(inicio)} – ${soloHora.format(fin)}`
    : `${completo.format(inicio)} – ${completo.format(fin)}`
}
