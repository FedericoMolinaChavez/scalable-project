import { useQuery } from '@tanstack/react-query'

import { consultaReservas } from '../api/consultas'
import { EstadoConsulta } from '../componentes/EstadoConsulta'

/**
 * Reservas del negocio: el alcance del ADMINISTRADOR (RF-32), no el del usuario.
 *
 * `GET /v1/reservas` no tiene un alcance fijo: sale del tipo de cuenta que la
 * llama (RF-23). Un usuario ve las suyas (RF-02), un administrador las de su
 * tenant. Sin autenticación (RF-12) no hay de dónde derivarlo, así que hoy
 * devuelve las del tenant entero.
 *
 * Esta pantalla lo dice en vez de disimularlo. Titularla «Mis reservas»
 * mientras enseña las de todo el mundo sería mentir sobre lo único que un
 * listado de reservas tiene que dejar claro: de quién son.
 *
 * Cuando exista RF-12 esto se parte en dos superficies distintas, no en una con
 * un filtro: la del usuario es una lista de lo suyo con OTP como alternativa al
 * login (RF-02); la del administrador es una agenda por sede y recurso, con
 * acciones sobre cada reserva —check-in, no-show, cancelar, modificar—.
 *
 * Estructura, no diseño: la identidad visual se define aparte.
 */
export function MisReservas() {
  const consulta = useQuery(consultaReservas())

  return (
    <section>
      <h1>Reservas del negocio</h1>

      <p>
        Sin autenticación, esta lista muestra las reservas de todo el tenant: el
        alcance del administrador (RF-32). La lista personal de cada cliente
        (RF-02) llega con RF-12.
      </p>

      <EstadoConsulta
        consulta={consulta}
        vacio={<p>Este negocio todavía no tiene reservas.</p>}
      >
        {(reservas) => (
          <ul>
            {reservas.map((reserva) => (
              <li key={reserva.id}>
                <span>{formatearPeriodo(reserva.periodo)}</span>
                <span> · {reserva.estado}</span>
                {reserva.contacto ? <span> · {reserva.contacto.nombre}</span> : null}
              </li>
            ))}
          </ul>
        )}
      </EstadoConsulta>
    </section>
  )
}

/**
 * El período es semiabierto `[inicio, fin)`, así que el instante final NO
 * pertenece a la reserva. Se muestra igualmente porque para una persona
 * "10:00–11:00" es lo natural, pero ningún cálculo debe tratar `fin` como
 * ocupado: es justo lo que permite que la cita siguiente empiece a las 11:00.
 *
 * Aquí sí se usa la zona del navegador, y es una deuda consciente: esta lista
 * mezcla reservas de varias sedes y cada una tiene la suya (RF-38), así que
 * formatear bien exige saber a qué sede pertenece cada fila. El contrato
 * todavía no trae la sede en la reserva. Ver src/formato/zona.ts, que sí lo
 * hace bien donde la sede se conoce.
 */
function formatearPeriodo(periodo: { inicio: string; fin: string }): string {
  const formato = new Intl.DateTimeFormat('es', { dateStyle: 'medium', timeStyle: 'short' })
  return `${formato.format(new Date(periodo.inicio))} – ${formato.format(new Date(periodo.fin))}`
}
