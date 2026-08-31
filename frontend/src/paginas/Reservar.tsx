import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ErrorApi, type Franja, type Reserva, type Sede, type Servicio } from '../api/cliente'
import { claves, consultaDisponibilidad, consultaSedes, consultaServicios } from '../api/consultas'
import { claveDeFranja, crearReserva, olvidarClave } from '../api/mutaciones'
import { EstadoConsulta, MensajeError } from '../componentes/EstadoConsulta'
import { finDelDia, hoyEn, inicioDelDia, periodo } from '../formato/zona'

/**
 * Reservar (RF-01 y RF-26).
 *
 * Es la rebanada vertical completa: de este componente al servicio de
 * disponibilidad, de ahí al núcleo y de ahí a la restricción EXCLUDE de
 * PostgreSQL. Lo que prueba no es la pantalla, es que las cuatro capas encajan.
 *
 * Estructura, no diseño: la identidad visual se define aparte.
 *
 * Lo que sí queda fijado aquí es el comportamiento que ninguna sesión de diseño
 * debería cambiar:
 *
 *   - Las horas se muestran en la zona de la SEDE, nunca en la del navegador
 *     (RF-38). Una cita de las 10:00 es a las 10:00 del reloj del negocio.
 *   - Un 409 no se reintenta. Significa que otra transacción se quedó con el
 *     cupo (RF-01, alt. 3): la salida es elegir otra franja, no insistir.
 *   - La disponibilidad se refresca tras un 409, porque acaba de quedar
 *     desactualizada de la peor manera: mostrando algo que ya no está.
 */
export function Reservar() {
  const sedes = useQuery(consultaSedes())
  const servicios = useQuery(consultaServicios())

  const [servicioId, setServicioId] = useState<string>('')
  const [fecha, setFecha] = useState<string>('')
  const [contacto, setContacto] = useState({ nombre: '', email: '' })
  const [creada, setCreada] = useState<Reserva | null>(null)

  const servicio = servicios.data?.find((s) => s.id === servicioId) ?? servicios.data?.[0]
  const sede = sedes.data?.find((s) => s.id === servicio?.sede_id)
  const zona = sede?.zona_horaria

  // La fecha por defecto es hoy EN LA SEDE, que no tiene por qué ser hoy aquí.
  // Se calcula al vuelo en vez de en un efecto: derivarlo del estado que ya
  // existe evita el parpadeo de un primer render con la fecha equivocada.
  const fechaElegida = fecha || (zona ? hoyEn(zona) : '')

  return (
    <section>
      <h1>Reservar</h1>

      <EstadoConsulta consulta={servicios} vacio={<p>Este negocio todavía no ofrece servicios.</p>}>
        {(lista) => (
          <>
            <Seleccion
              servicios={lista}
              servicioId={servicio?.id ?? ''}
              onServicio={(id) => {
                setServicioId(id)
                setCreada(null)
              }}
              fecha={fechaElegida}
              onFecha={(valor) => {
                setFecha(valor)
                setCreada(null)
              }}
            />

            <Contacto valor={contacto} onCambio={setContacto} />

            {servicio && zona ? (
              <Franjas
                servicio={servicio}
                sede={sede}
                zona={zona}
                fecha={fechaElegida}
                contacto={contacto}
                onCreada={setCreada}
              />
            ) : null}

            {creada && zona ? <Confirmacion reserva={creada} zona={zona} /> : null}
          </>
        )}
      </EstadoConsulta>
    </section>
  )
}

function Seleccion({
  servicios,
  servicioId,
  onServicio,
  fecha,
  onFecha,
}: {
  servicios: Servicio[]
  servicioId: string
  onServicio: (id: string) => void
  fecha: string
  onFecha: (fecha: string) => void
}) {
  return (
    <div>
      <p>
        <label htmlFor="servicio">Servicio</label>{' '}
        <select
          id="servicio"
          value={servicioId}
          onChange={(evento) => onServicio(evento.target.value)}
        >
          {servicios.map((servicio) => (
            <option key={servicio.id} value={servicio.id}>
              {servicio.nombre} · {servicio.duracion_min} min · {servicio.precio.monto}{' '}
              {servicio.precio.moneda}
            </option>
          ))}
        </select>
      </p>

      <p>
        <label htmlFor="fecha">Fecha</label>{' '}
        <input
          id="fecha"
          type="date"
          value={fecha}
          onChange={(evento) => onFecha(evento.target.value)}
        />
      </p>
    </div>
  )
}

function Contacto({
  valor,
  onCambio,
}: {
  valor: { nombre: string; email: string }
  onCambio: (valor: { nombre: string; email: string }) => void
}) {
  // Obligatorios porque sin autenticación toda reserva es de invitado, y el
  // esquema exige nombre y correo en ese caso (reserva_contacto_requerido):
  // sin ellos no habría forma de mandar la confirmación ni el recordatorio.
  return (
    <fieldset>
      <legend>Tus datos</legend>

      <p>
        <label htmlFor="nombre">Nombre</label>{' '}
        <input
          id="nombre"
          value={valor.nombre}
          onChange={(evento) => onCambio({ ...valor, nombre: evento.target.value })}
          required
        />
      </p>

      <p>
        <label htmlFor="email">Correo</label>{' '}
        <input
          id="email"
          type="email"
          value={valor.email}
          onChange={(evento) => onCambio({ ...valor, email: evento.target.value })}
          required
        />
      </p>
    </fieldset>
  )
}

function Franjas({
  servicio,
  sede,
  zona,
  fecha,
  contacto,
  onCreada,
}: {
  servicio: Servicio
  sede: Sede | undefined
  zona: string
  fecha: string
  contacto: { nombre: string; email: string }
  onCreada: (reserva: Reserva) => void
}) {
  const clienteConsultas = useQueryClient()

  const desde = inicioDelDia(fecha, zona).toISOString()
  const hasta = finDelDia(fecha, zona).toISOString()

  const disponibilidad = useQuery(consultaDisponibilidad(servicio.id, desde, hasta))

  const reservar = useMutation({
    mutationFn: (franja: Franja) =>
      crearReserva(
        {
          servicio_id: servicio.id,
          recurso_id: franja.recurso_id,
          periodo: franja.periodo,
          contacto: { nombre: contacto.nombre, email: contacto.email },
        },
        claveDeFranja(franja.recurso_id, franja.periodo.inicio),
      ),

    onSuccess: (reserva) => {
      onCreada(reserva)
      // La franja recién tomada ya no está libre, y la lista de reservas tiene
      // una más.
      void clienteConsultas.invalidateQueries({ queryKey: claves.disponibilidad(servicio.id, desde, hasta) })
      void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
    },

    onError: (error, franja) => {
      if (!(error instanceof ErrorApi) || !error.esHorarioOcupado) return

      // Perder la carrera invalida dos cosas a la vez. La lista de franjas,
      // porque acaba de mentir; y la clave de idempotencia de esta franja,
      // porque quedó atada a un intento que nunca creó nada.
      olvidarClave(franja.recurso_id, franja.periodo.inicio)
      void clienteConsultas.invalidateQueries({ queryKey: claves.disponibilidad(servicio.id, desde, hasta) })
    },
  })

  const datosCompletos = contacto.nombre.trim() !== '' && contacto.email.trim() !== ''

  return (
    <div>
      <h2>
        Horarios libres {sede ? `en ${sede.nombre}` : null}
      </h2>

      {/* La zona se dice en pantalla, no se da por supuesta. Sin decirlo, quien
          reserva desde otro huso no tiene forma de saber qué "10:00" está
          leyendo. */}
      <p>Horas de {zona}.</p>

      <EstadoConsulta
        consulta={disponibilidad}
        vacio={<p>No quedan horarios libres ese día.</p>}
        seleccionar={(datos) => datos.franjas}
      >
        {(franjas) => (
          <ul>
            {franjas.map((franja) => (
              <li key={`${franja.recurso_id}@${franja.periodo.inicio}`}>
                <button
                  type="button"
                  disabled={!datosCompletos || reservar.isPending}
                  onClick={() => reservar.mutate(franja)}
                >
                  {periodo(franja.periodo, zona)}
                </button>
              </li>
            ))}
          </ul>
        )}
      </EstadoConsulta>

      {!datosCompletos ? <p>Completa tu nombre y tu correo para poder reservar.</p> : null}

      {reservar.isError ? <MensajeError error={reservar.error} /> : null}
    </div>
  )
}

function Confirmacion({ reserva, zona }: { reserva: Reserva; zona: string }) {
  return (
    <div role="status">
      <h2>Horario apartado</h2>

      <p>
        {periodo(reserva.periodo, zona, true)} · {reserva.estado}
      </p>
      <p>
        {reserva.precio_cobrado.monto} {reserva.precio_cobrado.moneda}
      </p>

      {/* El bloqueo ES la reserva pendiente (RF-27), así que cuánto queda no es
          un detalle del flujo de pago: es lo que decide si el cupo sigue siendo
          suyo. */}
      {reserva.expira_en ? <Cuenta atras={reserva.expira_en} /> : null}
    </div>
  )
}

function Cuenta({ atras }: { atras: string }) {
  const [ahora, setAhora] = useState(() => Date.now())

  // Un intervalo por segundo y no un `setTimeout` al vencimiento: lo que hay
  // que mostrar es cuánto queda, no solo avisar al final.
  useEffect(() => {
    const id = setInterval(() => setAhora(Date.now()), 1_000)
    return () => clearInterval(id)
  }, [])

  const restante = Math.max(0, new Date(atras).getTime() - ahora)
  if (restante === 0) {
    return <p>El bloqueo venció. El horario ha vuelto a estar disponible.</p>
  }

  const minutos = Math.floor(restante / 60_000)
  const segundos = Math.floor((restante % 60_000) / 1_000)

  return (
    <p>
      Quedan{' '}
      <time dateTime={atras}>
        {minutos}:{String(segundos).padStart(2, '0')}
      </time>{' '}
      para completar el pago.
    </p>
  )
}
