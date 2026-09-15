import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router'

import { consultaSedes } from '../api/consultas'
import { EstadoConsulta, Vacio } from '../componentes/EstadoConsulta'

/**
 * Catálogo de sedes (RF-26).
 *
 * En el mundo de la cartera de billetes esto es el tablero de destinos: una
 * fila reglada por sede, con su zona horaria en cifra y su dirección debajo.
 * No son tarjetas — nada flota en esta aplicación — sino renglones separados
 * por filetes, que es como se imprime una lista de salidas.
 *
 * La zona horaria va en primera línea y no como dato menor: es del tenant y no
 * del navegador (RF-38), y una persona que reserva desde otro huso necesita
 * saber a qué reloj pertenece la hora que va a elegir.
 */
export function Catalogo() {
  const consulta = useQuery(consultaSedes())

  return (
    <section>
      <h1 className="titular mt-3 max-w-[14ch] text-[clamp(2.25rem,7vw,3.75rem)]">
        Elige dónde
      </h1>

      <p className="mt-4 max-w-[58ch] text-[0.9375rem] text-[var(--color-marina-media)]">
        Cada sede lleva su propia zona horaria. Todo lo que veas después —horarios libres,
        confirmaciones, comprobantes— está escrito en el reloj de la sede que elijas.
      </p>

      <div className="mt-10">
        <EstadoConsulta
          consulta={consulta}
          vacio={
            <Vacio titulo="Todavía no hay sedes">
              Este negocio aún no ha publicado ninguna sede, así que no hay nada que reservar.
              Vuelve más tarde.
            </Vacio>
          }
        >
          {(sedes) => (
            <ul className="border-t border-[var(--color-marina)]">
              {sedes.map((sede) => (
                <li
                  key={sede.id}
                  className="grid grid-cols-[1fr_auto] items-baseline gap-x-6 gap-y-1 border-b border-[var(--color-filete)] py-4"
                >
                  <span className="titular text-[1.375rem] tracking-[-0.02em]">
                    {sede.nombre}
                  </span>

                  <span className="cifra text-[0.8125rem]">{sede.zona_horaria}</span>

                  {sede.direccion ? (
                    <span className="col-span-2 text-[0.8125rem] text-[var(--color-marina-media)]">
                      {sede.direccion}
                    </span>
                  ) : null}
                </li>
              ))}
            </ul>
          )}
        </EstadoConsulta>
      </div>

      <div className="mt-10">
        <Link to="/reservar" className="boton no-underline">
          Ver horarios libres
        </Link>
      </div>
    </section>
  )
}
