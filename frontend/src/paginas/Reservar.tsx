import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'

import { ErrorApi, type Franja, type Reserva, type Sede, type Servicio } from '../api/cliente'
import { claves, consultaDisponibilidad, consultaSedes, consultaServicios } from '../api/consultas'
import { claveDeFranja, crearReserva, olvidarClave } from '../api/mutaciones'
import { Campo, Campos, Cupon, Estado, Marca, Pestana, Pestanas } from '../componentes/cupon'
import { EstadoConsulta, MensajeError, Vacio } from '../componentes/EstadoConsulta'
import { Pago } from '../componentes/Pago'
import { finDelDia, hora, hoyEn, inicioDelDia, periodo } from '../formato/zona'

/**
 * Reservar (RF-01 y RF-26).
 *
 * Es la rebanada vertical completa: de este componente al servicio de
 * disponibilidad, de ahí al núcleo y de ahí a la restricción EXCLUDE de
 * PostgreSQL. Lo que prueba no es la pantalla, es que las cuatro capas encajan.
 *
 * En el mundo de la cartera de billetes, esta pantalla es el mostrador: a la
 * izquierda los datos del pasajero, a la derecha el tablero de salidas con los
 * tramos libres. Elegir uno EMITE un cupón, con su reloj colgando de la tira
 * perforada, y el pago ocurre bajo ese reloj sin salir de la página.
 *
 * Lo que queda fijado aquí y ninguna sesión de diseño debería cambiar:
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

  // Una vez emitido el cupón, la pantalla es el cupón. Seguir enseñando el
  // tablero de salidas debajo invitaría a elegir otra franja mientras corre el
  // reloj de la que ya está apartada, y eso es inventario propio denegado.
  if (creada && zona && servicio) {
    return (
      <Confirmacion
        reserva={creada}
        servicio={servicio}
        sede={sede}
        zona={zona}
        onOtra={() => setCreada(null)}
      />
    )
  }

  return (
    <section>
      <h1 className="titular mt-3 max-w-[16ch] text-[clamp(2.25rem,7vw,3.75rem)]">
        Elige tu hora
      </h1>

      <div className="mt-10">
        <EstadoConsulta
          consulta={servicios}
          vacio={
            <Vacio titulo="Este negocio todavía no ofrece servicios">
              Sin servicios publicados no hay disponibilidad que calcular.
            </Vacio>
          }
        >
          {(lista) => (
            <div className="grid gap-x-10 gap-y-10 lg:grid-cols-[minmax(0,22rem)_minmax(0,1fr)]">
              <div className="flex flex-col gap-8">
                <Seleccion
                  servicios={lista}
                  servicioId={servicio?.id ?? ''}
                  onServicio={setServicioId}
                  fecha={fechaElegida}
                  onFecha={setFecha}
                />

                <Contacto valor={contacto} onCambio={setContacto} />
              </div>

              {servicio && zona ? (
                <Tablero
                  servicio={servicio}
                  sede={sede}
                  zona={zona}
                  fecha={fechaElegida}
                  contacto={contacto}
                  onCreada={setCreada}
                />
              ) : null}
            </div>
          )}
        </EstadoConsulta>
      </div>
    </section>
  )
}

/**
 * El cupón del servicio: qué se está comprando, en pestañas y campos.
 *
 * Servicio y fecha son los dos mandos de esta pantalla y viven dentro del
 * cupón, no encima de él: en una cartera de billetes los datos del viaje se
 * escriben sobre el propio cupón.
 */
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
  const elegido = servicios.find((s) => s.id === servicioId) ?? servicios[0]

  return (
    <div>
      <Pestanas>
        <Pestana>Servicio</Pestana>
        <Pestana clara>Fecha</Pestana>
      </Pestanas>

      <Cupon>
        <div className="p-4">
          <label htmlFor="servicio" className="etiqueta">
            Qué reservas
          </label>
          <select
            id="servicio"
            value={servicioId}
            onChange={(evento) => onServicio(evento.target.value)}
            className="entrada mt-1.5 font-[var(--font-cuerpo)]"
          >
            {servicios.map((servicio) => (
              <option key={servicio.id} value={servicio.id}>
                {servicio.nombre}
              </option>
            ))}
          </select>

          <label htmlFor="fecha" className="etiqueta mt-4 block">
            Qué día
          </label>
          <input
            id="fecha"
            type="date"
            value={fecha}
            onChange={(evento) => onFecha(evento.target.value)}
            className="entrada mt-1.5"
          />
        </div>

        {elegido ? (
          <Campos columnas="1fr 1fr">
            <Campo etiqueta="Duración" cifra>
              {elegido.duracion_min} min
            </Campo>
            <Campo etiqueta="Precio" cifra>
              {elegido.precio.monto} {elegido.precio.moneda}
            </Campo>
          </Campos>
        ) : null}
      </Cupon>
    </div>
  )
}

/**
 * Los datos del pasajero.
 *
 * Obligatorios porque sin autenticación toda reserva es de invitado, y el
 * esquema exige nombre y correo en ese caso (reserva_contacto_requerido): sin
 * ellos no habría forma de mandar la confirmación ni el recordatorio.
 *
 * Se dice POR QUÉ hacen falta en vez de marcarlos con un asterisco. Un
 * asterisco pide un dato; una frase explica qué se hace con él.
 */
function Contacto({
  valor,
  onCambio,
}: {
  valor: { nombre: string; email: string }
  onCambio: (valor: { nombre: string; email: string }) => void
}) {
  return (
    <div>
      <Pestanas>
        <Pestana>Pasajero</Pestana>
      </Pestanas>

      <Cupon>
        <div className="p-4">
          <label htmlFor="nombre" className="etiqueta">
            Nombre
          </label>
          <input
            id="nombre"
            value={valor.nombre}
            onChange={(evento) => onCambio({ ...valor, nombre: evento.target.value })}
            className="entrada mt-1.5"
            autoComplete="name"
            required
          />

          <label htmlFor="email" className="etiqueta mt-4 block">
            Correo
          </label>
          <input
            id="email"
            type="email"
            value={valor.email}
            onChange={(evento) => onCambio({ ...valor, email: evento.target.value })}
            className="entrada mt-1.5"
            autoComplete="email"
            required
          />

          <p className="mt-3 text-[0.75rem] leading-relaxed text-[var(--color-marina-media)]">
            Con este correo te llega la confirmación, y es también con el que podrás volver a
            ver o cancelar la reserva sin tener cuenta.
          </p>
        </div>
      </Cupon>
    </div>
  )
}

/**
 * El tablero de salidas: los tramos libres del día.
 *
 * Filas regladas, la hora en cifra grande a la izquierda, el estado en chip
 * contorneado a la derecha. Es la lectura que este producto necesita —comparar
 * horas de un vistazo contra una agenda que está fuera de la pantalla— y es
 * literalmente el tablero de estado en vivo del mundo elegido.
 *
 * Cada fila ES el botón. Un botón separado dentro de la fila obligaría a
 * apuntar dos veces: primero leer la hora, luego buscar dónde pulsar.
 */
function Tablero({
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
      void clienteConsultas.invalidateQueries({
        queryKey: claves.disponibilidad(servicio.id, desde, hasta),
      })
      void clienteConsultas.invalidateQueries({ queryKey: claves.reservas() })
    },

    onError: (error, franja) => {
      if (!(error instanceof ErrorApi) || !error.esHorarioOcupado) return

      // Perder la carrera invalida dos cosas a la vez. La lista de franjas,
      // porque acaba de mentir; y la clave de idempotencia de esta franja,
      // porque quedó atada a un intento que nunca creó nada.
      olvidarClave(franja.recurso_id, franja.periodo.inicio)
      void clienteConsultas.invalidateQueries({
        queryKey: claves.disponibilidad(servicio.id, desde, hasta),
      })
    },
  })

  const datosCompletos = contacto.nombre.trim() !== '' && contacto.email.trim() !== ''

  return (
    <div>
      <div className="flex flex-wrap items-baseline justify-between gap-x-6 gap-y-1 border-b border-[var(--color-marina)] pb-2">
        <Marca>Horarios libres{sede ? ` · ${sede.nombre}` : ''}</Marca>

        {/* La zona se dice en pantalla, no se da por supuesta. Sin decirlo,
            quien reserva desde otro huso no tiene forma de saber qué "10:00"
            está leyendo. */}
        <span className="cifra text-[0.75rem]">{zona}</span>
      </div>

      {!datosCompletos ? (
        <p className="mt-3 text-[0.8125rem] text-[var(--color-marina-media)]">
          Completa tu nombre y tu correo para poder apartar una hora.
        </p>
      ) : null}

      <div className="mt-4">
        <EstadoConsulta
          consulta={disponibilidad}
          vacio={
            <Vacio titulo="No quedan horarios libres ese día">
              Prueba otro día, u otro servicio. La disponibilidad se calcula sobre las reglas de
              la sede menos sus excepciones y lo que ya está reservado.
            </Vacio>
          }
          seleccionar={(datos) => datos.franjas}
        >
          {(franjas) => (
            <ul className="border-t border-[var(--color-filete)]">
              {franjas.map((franja) => (
                <li key={`${franja.recurso_id}@${franja.periodo.inicio}`}>
                  <button
                    type="button"
                    disabled={!datosCompletos || reservar.isPending}
                    onClick={() => reservar.mutate(franja)}
                    // El nombre accesible se escribe entero y no se deja
                    // derivar del contenido. Leída en voz alta, la fila visual
                    // suena "09:00 10:00 Apartar": dos cifras sueltas y un
                    // verbo, sin decir qué es cada una. La etiqueta dice la
                    // acción y el tramo en una frase, que es lo que alguien
                    // necesita oír antes de pulsar algo que aparta inventario.
                    aria-label={`Apartar de ${hora(franja.periodo.inicio, zona)} a ${hora(
                      franja.periodo.fin,
                      zona,
                    )}`}
                    className="group grid w-full grid-cols-[auto_1fr_auto] items-center gap-4 border-b border-[var(--color-filete)] px-1 py-3 text-left transition-colors hover:bg-[var(--color-cupon)] disabled:cursor-not-allowed disabled:opacity-45 disabled:hover:bg-transparent"
                  >
                    <span className="cifra text-[1.25rem] leading-none tracking-[-0.02em] sm:text-[1.5rem]">
                      {hora(franja.periodo.inicio, zona)}
                    </span>

                    {/* El guion largo entre horas es del mundo del billete: un
                        tramo, no dos datos sueltos. La hora final va más
                        pequeña porque el instante final NO pertenece a la
                        reserva, y la jerarquía lo dice sin una nota al pie. */}
                    <span className="cifra text-[0.8125rem] text-[var(--color-marina-media)]">
                      <span aria-hidden="true">— </span>
                      {hora(franja.periodo.fin, zona)}
                    </span>

                    <span className="estado estado--libre transition-colors group-hover:border-[var(--color-rojo)] group-hover:text-[var(--color-rojo)]">
                      Apartar
                    </span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </EstadoConsulta>
      </div>

      {reservar.isError ? (
        <div className="mt-6">
          <MensajeError error={reservar.error} />
        </div>
      ) : null}
    </div>
  )
}

/**
 * El cupón emitido: lo que se apartó, su reloj, y el pago.
 *
 * El orden no es arbitrario. Primero QUÉ se apartó, después CUÁNTO QUEDA, y
 * solo entonces el formulario de pago. El reloj va encima del formulario porque
 * es lo que decide si vale la pena empezar a rellenarlo, y alguien con prisa lee
 * de arriba abajo una sola vez.
 */
function Confirmacion({
  reserva,
  servicio,
  sede,
  zona,
  onOtra,
}: {
  reserva: Reserva
  servicio: Servicio
  sede: Sede | undefined
  zona: string
  onOtra: () => void
}) {
  // El estado local existe porque la reserva que se enseña la devolvió el
  // núcleo hace un momento y no se vuelve a pedir: cuando el webhook la
  // confirme (RF-33), quien se entera es el componente de pago, y esto es lo
  // que traduce ese aviso a lo que se ve.
  const [confirmada, setConfirmada] = useState(false)

  return (
    <section>
      <h1 className="titular mt-3 max-w-[18ch] text-[clamp(2rem,6vw,3.25rem)]">
        {confirmada ? 'La hora es tuya' : 'Te la guardamos mientras pagas'}
      </h1>

      <div className="mt-8 grid gap-x-10 gap-y-8 lg:grid-cols-[minmax(0,26rem)_minmax(0,1fr)]">
        <div>
          <Pestanas>
            <Pestana>{sede?.nombre ?? 'Sede'}</Pestana>
            <Pestana clara>{servicio.nombre}</Pestana>
          </Pestanas>

          {/* La emisión: la única animación coreografiada del sistema, y ocurre
              una sola vez, en el instante en que el núcleo devuelve la reserva. */}
          <Cupon emitido className="flex">
            <div className="min-w-0 flex-1">
              <div className="flex flex-wrap items-baseline justify-between gap-x-4 gap-y-2 p-4">
                <span className="cifra text-[1.5rem] leading-none tracking-[-0.02em]">
                  {periodo(reserva.periodo, zona, true)}
                </span>
                <Estado estado={confirmada ? 'confirmada' : reserva.estado} />
              </div>

              <Campos columnas="1fr 1fr">
                <Campo etiqueta="Precio" cifra>
                  {reserva.precio_cobrado.monto} {reserva.precio_cobrado.moneda}
                </Campo>
                <Campo etiqueta="Zona" cifra>
                  {zona}
                </Campo>
                <Campo etiqueta="Referencia" cifra>
                  <span className="text-[0.75rem] break-all">{reserva.id.slice(0, 13)}</span>
                </Campo>
                <Campo etiqueta="Duración" cifra>
                  {servicio.duracion_min} min
                </Campo>
              </Campos>
            </div>

            {/* La tira perforada marca lo provisional. Cuando el pago se
                confirma, deja de estar ahí: ya no hay nada que arrancar. */}
            {!confirmada ? <div className="tira w-2.5 shrink-0" aria-hidden="true" /> : null}
          </Cupon>

          {/* El bloqueo ES la reserva pendiente (RF-27), así que cuánto queda no
              es un detalle del flujo de pago: es lo que decide si el cupo sigue
              siendo suyo. */}
          {!confirmada && reserva.expira_en ? <Cuenta atras={reserva.expira_en} /> : null}

          {confirmada ? (
            <p className="mt-4 max-w-[52ch] text-[0.875rem] text-[var(--color-marina-media)]">
              Te llega la confirmación al correo. El comprobante aparecerá en «Mis reservas» en
              cuanto se emita, unos segundos después del cobro.
            </p>
          ) : null}
        </div>

        <div>
          {confirmada ? (
            <button type="button" className="boton boton--secundario" onClick={onOtra}>
              Reservar otra hora
            </button>
          ) : (
            <>
              <div className="border-b border-[var(--color-marina)] pb-2">
                <Marca>Pago</Marca>
              </div>
              <div className="mt-4">
                <Pago reserva={reserva} onConfirmada={() => setConfirmada(true)} />
              </div>
            </>
          )}
        </div>
      </div>
    </section>
  )
}

/**
 * El reloj de RF-27.
 *
 * No es una alarma. Es la tira perforada del cupón vaciándose: una barra que
 * avanza de forma continua y unas cifras tabulares que no tiemblan porque son
 * de ancho fijo. La desviación se inclina ANTES de alarmar — el rojo aparece en
 * el último minuto, cuando ya es información y no presión.
 *
 * Un intervalo por segundo y no un `setTimeout` al vencimiento: lo que hay que
 * mostrar es cuánto queda, no solo avisar al final.
 */
function Cuenta({ atras }: { atras: string }) {
  const [ahora, setAhora] = useState(() => Date.now())
  const [inicio] = useState(() => Date.now())

  useEffect(() => {
    const id = setInterval(() => setAhora(Date.now()), 1_000)
    return () => clearInterval(id)
  }, [])

  const vence = new Date(atras).getTime()
  const restante = Math.max(0, vence - ahora)

  if (restante === 0) {
    return (
      <div
        role="status"
        className="mt-4 flex items-center gap-3 border border-[var(--color-rojo)] bg-[var(--color-cupon)] p-3"
      >
        <span className="sello estampado">Vencida</span>
        <p className="text-[0.8125rem] text-[var(--color-marina-media)]">
          El bloqueo venció y el horario volvió a estar libre. Puedes elegir otra hora.
        </p>
      </div>
    )
  }

  const total = Math.max(1, vence - inicio)
  const restantePct = Math.min(100, (restante / total) * 100)

  const minutos = Math.floor(restante / 60_000)
  const segundos = Math.floor((restante % 60_000) / 1_000)

  // El último minuto. Antes de eso la barra ya se ha ido acortando a la vista
  // durante catorce minutos, así que el rojo no es una sorpresa: es el final de
  // una tendencia que se venía leyendo.
  const apurado = restante < 60_000

  return (
    <div className="mt-4">
      <div className="flex items-baseline justify-between gap-4">
        <span className="etiqueta">Queda para pagar</span>
        <time
          dateTime={atras}
          className={`cifra text-[1.125rem] tabular-nums ${
            apurado ? 'text-[var(--color-rojo)]' : ''
          }`}
        >
          {minutos}:{String(segundos).padStart(2, '0')}
        </time>
      </div>

      <div className="mt-1.5 h-1 bg-[var(--color-filete)]">
        <div
          className={`h-1 transition-[width] duration-1000 ease-linear ${
            apurado ? 'bg-[var(--color-rojo)]' : 'bg-[var(--color-carbon)]'
          }`}
          style={{ width: `${restantePct}%` }}
          aria-hidden="true"
        />
      </div>

      <p className="mt-2 text-[0.75rem] text-[var(--color-marina-media)]">
        Si vence, el horario vuelve al mercado y no se te cobra nada.
      </p>
    </div>
  )
}
