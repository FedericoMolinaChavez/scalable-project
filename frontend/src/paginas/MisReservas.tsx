import { useQuery } from '@tanstack/react-query'

import { consultaReservas } from '../api/consultas'
import { EstadoConsulta } from '../componentes/EstadoConsulta'

/** Reservas de la cuenta (RF-02). */
export function MisReservas() {
  const consulta = useQuery(consultaReservas())

  return (
    <section>
      <h1>Mis reservas</h1>

      <EstadoConsulta consulta={consulta} vacio={<p>Todavía no tienes reservas.</p>}>
        {(reservas) => (
          <ul>
            {reservas.map((reserva) => (
              <li key={reserva.id}>
                <span>{formatearPeriodo(reserva.periodo)}</span>
                <span> · {reserva.estado}</span>
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
 */
function formatearPeriodo(periodo: { inicio: string; fin: string }): string {
  const formato = new Intl.DateTimeFormat('es', { dateStyle: 'medium', timeStyle: 'short' })
  return `${formato.format(new Date(periodo.inicio))} – ${formato.format(new Date(periodo.fin))}`
}
