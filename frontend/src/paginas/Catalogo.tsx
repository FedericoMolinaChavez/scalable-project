import { useQuery } from '@tanstack/react-query'

import { consultaSedes } from '../api/consultas'
import { EstadoConsulta } from '../componentes/EstadoConsulta'

/**
 * Catálogo de sedes (RF-26).
 *
 * Estructura, no diseño: la identidad visual se define en una sesión aparte.
 * Lo que esta pantalla ya deja fijado es que los datos vienen del contrato
 * generado y que los tres estados de la consulta están cubiertos.
 */
export function Catalogo() {
  const consulta = useQuery(consultaSedes())

  return (
    <section>
      <h1>Sedes</h1>

      <EstadoConsulta consulta={consulta} vacio={<p>Todavía no hay sedes.</p>}>
        {(sedes) => (
          <ul>
            {sedes.map((sede) => (
              <li key={sede.id}>
                <span>{sede.nombre}</span>
                <span> · {sede.zona_horaria}</span>
                {sede.direccion ? <span> · {sede.direccion}</span> : null}
              </li>
            ))}
          </ul>
        )}
      </EstadoConsulta>
    </section>
  )
}
