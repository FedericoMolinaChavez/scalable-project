import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'

import { ErrorApi, type Reserva } from '../api/cliente'
import { claves, consultaReservas } from '../api/consultas'
import { cancelarReserva } from '../api/mutaciones'
import { canjearCodigo, olvidarSesion, sesionGuardada, solicitarCodigo } from '../api/sesion'
import { EstadoConsulta, MensajeError } from '../componentes/EstadoConsulta'

/**
 * Mis reservas (RF-02) y cancelación (RF-06).
 *
 * Dos pantallas en una, porque son dos estados del mismo sitio: sin
 * identificarse se pide un código; con el token, se ven las reservas propias.
 *
 * Estructura, no diseño: la identidad visual se define aparte.
 *
 * Lo que sí queda fijado:
 *
 *   - La pantalla NUNCA dice si un correo existe. Tras pedir el código responde
 *     lo mismo en todos los casos (RF-12 A12), porque una respuesta que los
 *     distinga convierte esto en un buscador de clientes del negocio.
 *   - Un 401 no se trata como un error más: significa que la identificación
 *     caducó, y la salida es volver a pedir un código, no reintentar.
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
      <h1>Mis reservas</h1>

      <p>
        Para ver tus reservas te mandamos un código al correo con el que
        reservaste.
      </p>

      <form
        onSubmit={(evento) => {
          evento.preventDefault()
          if (enviado) canjear.mutate()
          else pedir.mutate()
        }}
      >
        <p>
          <label htmlFor="destino">Correo</label>{' '}
          <input
            id="destino"
            type="email"
            value={destino}
            onChange={(evento) => setDestino(evento.target.value)}
            // Cambiar el correo después de pedir el código vuelve al principio:
            // el código que llegó es del correo anterior.
            readOnly={enviado}
            required
          />
        </p>

        {enviado ? (
          <>
            {/* El mensaje no afirma que el correo exista: dice qué pasa SI
                existe. Es la misma frase para una dirección con reservas y para
                una inventada. */}
            <p role="status">
              Si hay reservas hechas con ese correo, el código ya va en camino.
              Caduca en unos minutos.
            </p>

            <p>
              <label htmlFor="codigo">Código</label>{' '}
              <input
                id="codigo"
                inputMode="numeric"
                pattern="[0-9]{6}"
                maxLength={6}
                value={codigo}
                onChange={(evento) => setCodigo(evento.target.value.replace(/\D/g, ''))}
                required
              />
            </p>
          </>
        ) : null}

        <p>
          <button type="submit" disabled={pedir.isPending || canjear.isPending}>
            {enviado ? 'Entrar' : 'Enviarme el código'}
          </button>{' '}
          {enviado ? (
            <button
              type="button"
              onClick={() => {
                setEnviado(false)
                setCodigo('')
                canjear.reset()
              }}
            >
              Usar otro correo
            </button>
          ) : null}
        </p>
      </form>

      {pedir.isError ? <MensajeError error={pedir.error} /> : null}
      {canjear.isError ? <MensajeError error={canjear.error} /> : null}
    </section>
  )
}

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
      <h1>Mis reservas</h1>

      <p>
        <button
          type="button"
          onClick={() => {
            olvidarSesion()
            void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
            onCaducada()
          }}
        >
          Salir
        </button>
      </p>

      <EstadoConsulta
        consulta={consulta}
        vacio={<p>No hay reservas hechas con ese correo.</p>}
      >
        {(reservas) => (
          <ul>
            {reservas.map((reserva) => (
              <li key={reserva.id}>
                <Fila reserva={reserva} onCaducada={onCaducada} />
              </li>
            ))}
          </ul>
        )}
      </EstadoConsulta>
    </section>
  )
}

function Fila({ reserva, onCaducada }: { reserva: Reserva; onCaducada: () => void }) {
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

  return (
    <>
      <span>{formatearPeriodo(reserva.periodo)}</span>
      <span> · {reserva.estado}</span>
      <span>
        {' '}
        · {reserva.precio_cobrado.monto} {reserva.precio_cobrado.moneda}
      </span>

      {cancelable ? (
        <>
          {' '}
          <button type="button" disabled={cancelar.isPending} onClick={() => cancelar.mutate()}>
            Cancelar
          </button>
        </>
      ) : null}

      {/* El motivo del rechazo va junto a la reserva que lo provocó, no en una
          alerta global: con varias reservas en pantalla, un mensaje suelto no
          dice a cuál se refiere. */}
      {cancelar.isError ? <MensajeError error={cancelar.error} /> : null}
    </>
  )
}

/**
 * El período es semiabierto `[inicio, fin)`, así que el instante final NO
 * pertenece a la reserva. Se muestra igualmente porque para una persona
 * "10:00–11:00" es lo natural, pero ningún cálculo debe tratar `fin` como
 * ocupado: es justo lo que permite que la cita siguiente empiece a las 11:00.
 *
 * Aquí se usa la zona del navegador, y es una deuda consciente: esta lista
 * mezcla reservas de varias sedes y cada una tiene la suya (RF-38), así que
 * formatear bien exige saber a qué sede pertenece cada fila. El contrato
 * todavía no trae la sede en la reserva. Ver src/formato/zona.ts, que sí lo
 * hace bien donde la sede se conoce.
 */
function formatearPeriodo(periodo: { inicio: string; fin: string }): string {
  const formato = new Intl.DateTimeFormat('es', { dateStyle: 'medium', timeStyle: 'short' })
  return `${formato.format(new Date(periodo.inicio))} – ${formato.format(new Date(periodo.fin))}`
}
