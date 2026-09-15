import type { UseQueryResult } from '@tanstack/react-query'
import type { ReactNode } from 'react'

import { ErrorApi } from '../api/cliente'

/**
 * Los tres estados que toda consulta tiene, resueltos en un sitio.
 *
 * Existe para que ningún componente muestre datos sin haber contemplado el
 * caso de carga y el de error. Escrito a mano en cada pantalla, el estado que
 * siempre falta es el de error, y la interfaz se queda en blanco sin explicar
 * nada.
 */
export function EstadoConsulta<D, T = D extends (infer E)[] ? E : never>({
  consulta,
  vacio,
  seleccionar,
  children,
}: {
  consulta: UseQueryResult<D>
  vacio: ReactNode

  /**
   * De dónde sacar la lista cuando la respuesta no es una lista.
   *
   * `/v1/disponibilidad` no devuelve un array, devuelve un objeto con
   * `franjas` dentro y la marca de cuándo se calculó. Sin esto, esa pantalla
   * tendría que resolver los tres estados a mano, y el estado que siempre falta
   * escrito a mano es el de error.
   *
   * Omitirlo solo vale cuando la respuesta YA es la lista.
   */
  seleccionar?: (datos: D) => T[]
  children: (datos: T[]) => ReactNode
}) {
  if (consulta.isPending) {
    return <Cargando />
  }

  if (consulta.isError) {
    return <MensajeError error={consulta.error} onReintentar={() => void consulta.refetch()} />
  }

  // Sin `seleccionar`, la respuesta es la lista. El tipo por defecto de T lo
  // deriva del elemento de D, así que quien pase una consulta que no devuelve
  // un array y omita `seleccionar` obtiene T = never y no le compila el
  // `children`. La conversión de aquí es el precio de que TypeScript no sepa
  // expresar "esta rama solo se alcanza cuando D es T[]".
  const lista = seleccionar ? seleccionar(consulta.data) : (consulta.data as unknown as T[])

  if (lista.length === 0) {
    return <>{vacio}</>
  }

  return <>{children(lista)}</>
}

/**
 * La espera.
 *
 * Tres filetes que se rellenan de izquierda a derecha, con un desfase: es el
 * papel entrando en la máquina, no un disco girando. Un disco de carga es un
 * componente de librería que no pertenece a ningún sistema; esto está hecho de
 * las mismas reglas que el resto de la página.
 */
function Cargando() {
  return (
    <div className="py-8" role="status" aria-busy="true">
      <span className="sr-only">Cargando…</span>

      <div className="flex max-w-md flex-col gap-2" aria-hidden="true">
        {[0, 1, 2].map((fila) => (
          <div key={fila} className="h-px overflow-hidden bg-[var(--color-filete)]">
            <div
              className="h-px bg-[var(--color-carbon)]"
              style={{
                animation: `avanzar 1.4s var(--salida) ${fila * 0.18}s infinite`,
              }}
            />
          </div>
        ))}
      </div>
    </div>
  )
}

/**
 * El error.
 *
 * No es una tarjeta con un icono de advertencia: es un cupón anulado. El sello
 * dice qué clase de problema fue, el cuerpo dice qué pasó y, cuando lo hay, el
 * detalle del problema (RFC 9457) explica esta ocurrencia concreta — el título
 * solo dice de qué tipo es.
 */
export function MensajeError({
  error,
  onReintentar,
}: {
  error: unknown
  onReintentar?: () => void
}) {
  const esApi = error instanceof ErrorApi

  return (
    <div
      role="alert"
      className="max-w-xl border border-[var(--color-rojo)] bg-[var(--color-cupon)]"
    >
      <div className="tira h-1.5" aria-hidden="true" />

      <div className="p-4">
        <p className="titular text-[1.0625rem] text-[var(--color-rojo)]">
          {esApi ? error.message : 'No se pudo completar la operación'}
        </p>

        {esApi && error.problema?.detail ? (
          <p className="mt-2 max-w-[60ch] text-[0.875rem] text-[var(--color-marina-media)]">
            {error.problema.detail}
          </p>
        ) : null}

        {onReintentar ? (
          <button type="button" className="boton boton--secundario mt-4" onClick={onReintentar}>
            Reintentar
          </button>
        ) : null}
      </div>
    </div>
  )
}

/**
 * El vacío.
 *
 * Un vacío no es un fallo, así que no lleva sello ni rojo: es un cupón en
 * blanco, con su filete y su explicación. Lo que no puede hacer es quedarse en
 * una frase suelta sin forma, que es lo que convierte una lista vacía en la
 * sensación de que algo se rompió.
 */
export function Vacio({ titulo, children }: { titulo: string; children?: ReactNode }) {
  return (
    <div className="max-w-xl border border-dashed border-[var(--color-filete)] bg-[var(--color-cupon)] p-6">
      <p className="titular text-[1.0625rem] text-[var(--color-marina)]">{titulo}</p>
      {children ? (
        <p className="mt-2 max-w-[60ch] text-[0.875rem] text-[var(--color-marina-media)]">
          {children}
        </p>
      ) : null}
    </div>
  )
}
