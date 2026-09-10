import type { ReactNode } from 'react'

import type { EstadoReserva } from '../api/cliente'

/**
 * El vocabulario del cupón.
 *
 * Todo lo que esta aplicación enseña es un cupón de una cartera de billetes:
 * una cabecera de pestañas, una rejilla de campos reglados a filete, y un
 * estado que se sella encima. Estas piezas existen para que no haya dos
 * dialectos —una reserva no se puede ver distinta en «reservar» y en «mis
 * reservas»— y para que el bloque de información sea SIEMPRE el mismo:
 * sede · servicio · recurso · tramo · estado · precio.
 *
 * Ninguna de ellas lleva sombra. La regla del sistema es que nada flota: los
 * elementos se separan con filetes, como los campos impresos de un cupón.
 */

/** Una pestaña de la cabecera del cupón. */
export function Pestana({ children, clara }: { children: ReactNode; clara?: boolean }) {
  return <span className={clara ? 'pestana pestana--clara' : 'pestana'}>{children}</span>
}

/** La cabecera: la fila de pestañas que corona un cupón. */
export function Pestanas({ children }: { children: ReactNode }) {
  return <div className="pestanas">{children}</div>
}

/**
 * El cuerpo del cupón.
 *
 * `emitido` enciende la única animación coreografiada del sistema: el cupón
 * saliendo de la máquina. Se usa exactamente una vez, cuando el núcleo devuelve
 * la reserva recién creada; ponerla en cada render convertiría un momento en un
 * tic.
 */
export function Cupon({
  children,
  emitido = false,
  className = '',
}: {
  children: ReactNode
  emitido?: boolean
  className?: string
}) {
  return <div className={`cupon ${emitido ? 'emitido' : ''} ${className}`}>{children}</div>
}

/**
 * La rejilla de campos.
 *
 * El fondo de la rejilla ES el filete: se pinta el separador como color de
 * fondo y cada campo se recorta encima con un hueco de 1px. Así los filetes son
 * continuos y de grosor exacto, en vez de sumarse en las intersecciones como
 * hacen los bordes por celda.
 */
export function Campos({
  children,
  columnas = 'repeat(auto-fit, minmax(7.5rem, 1fr))',
}: {
  children: ReactNode
  columnas?: string
}) {
  return (
    <div className="campos" style={{ gridTemplateColumns: columnas }}>
      {children}
    </div>
  )
}

/**
 * Un campo del cupón: etiqueta arriba en versalitas, valor abajo.
 *
 * `cifra` decide la tinta. Lo que la máquina MIDIÓ —una hora, un importe, una
 * referencia— va en mono de carbón violeta; lo que alguien escribió va en la
 * grotesca en azul. La distinción no es estética: separa el dato registrado del
 * texto libre.
 */
export function Campo({
  etiqueta,
  children,
  cifra = false,
}: {
  etiqueta: string
  children: ReactNode
  cifra?: boolean
}) {
  return (
    <div className="campo">
      <div className="etiqueta">{etiqueta}</div>
      <div className={cifra ? 'cifra text-[0.9375rem]' : 'text-[0.9375rem] font-medium'}>
        {children}
      </div>
    </div>
  )
}

/**
 * El estado de una reserva, en el vocabulario del modelo (RF-28).
 *
 * Los siete estados del enum, sin traducir ni agrupar: PRODUCT.md dice que si
 * un término no sirve para una persona se cambia en el modelo y en la interfaz
 * a la vez, nunca solo en la pantalla.
 *
 * El color es el segundo canal, no el primero. La palabra va siempre, así que
 * esto se lee igual en escala de grises, con un lector de pantalla, y a pleno
 * sol en un bus. Los dos estados en que la reserva ya no existe llevan además
 * el sello: nada desaparece de esta interfaz, se anula.
 */
const tonoPorEstado: Record<EstadoReserva, string> = {
  pendiente: 'estado--pendiente',
  confirmada: 'estado--conforme',
  en_curso: 'estado--conforme',
  completada: 'estado--inerte',
  cancelada: 'estado--anulado',
  no_show: 'estado--anulado',
  expirada: 'estado--anulado',
}

/** Los estados en que el cupón ya no vale, que es cuando se sella. */
const anulados: ReadonlySet<EstadoReserva> = new Set(['cancelada', 'expirada', 'no_show'])

export function Estado({ estado }: { estado: EstadoReserva }) {
  if (anulados.has(estado)) {
    return (
      <span className="sello" role="status">
        {estado === 'no_show' ? 'No show' : estado}
      </span>
    )
  }

  return <span className={`estado ${tonoPorEstado[estado]}`}>{estado.replace('_', ' ')}</span>
}

/**
 * La tira perforada: el borde por el que se arranca un cupón.
 *
 * Marca lo PROVISIONAL. Una reserva pendiente es un bloqueo con reloj (RF-27),
 * y en este mundo lo que cuelga de una perforación es justo lo que todavía se
 * puede arrancar. Cuando el pago se confirma, la tira desaparece.
 */
export function Tira() {
  return <div className="tira w-2 shrink-0" aria-hidden="true" />
}

/** La marca de sección: `/// ETIQUETA`. Es el encabezado, no un antetítulo. */
export function Marca({ children }: { children: ReactNode }) {
  return <h2 className="marca">{children}</h2>
}
