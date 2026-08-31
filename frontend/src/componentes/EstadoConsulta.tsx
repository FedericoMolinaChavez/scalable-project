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
export function EstadoConsulta<T>({
  consulta,
  vacio,
  children,
}: {
  consulta: UseQueryResult<T[]>
  vacio: ReactNode
  children: (datos: T[]) => ReactNode
}) {
  if (consulta.isPending) {
    return <p aria-busy="true">Cargando…</p>
  }

  if (consulta.isError) {
    return <MensajeError error={consulta.error} onReintentar={() => void consulta.refetch()} />
  }

  if (consulta.data.length === 0) {
    return <>{vacio}</>
  }

  return <>{children(consulta.data)}</>
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
