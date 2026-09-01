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
 *
 * La presentación es deliberadamente mínima: la identidad visual se define
 * aparte. Lo que aquí importa es que los tres caminos existan.
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
    return <p aria-busy="true">Cargando…</p>
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

export function MensajeError({
  error,
  onReintentar,
}: {
  error: unknown
  onReintentar?: () => void
}) {
  const esApi = error instanceof ErrorApi

  return (
    <div role="alert">
      <p>{esApi ? error.message : 'No se pudo completar la operación.'}</p>

      {/* El detalle del problema (RFC 9457) explica esta ocurrencia concreta;
          el título solo dice de qué tipo es. */}
      {esApi && error.problema?.detail ? <p>{error.problema.detail}</p> : null}

      {onReintentar ? (
        <button type="button" onClick={onReintentar}>
          Reintentar
        </button>
      ) : null}
    </div>
  )
}
