import { Link } from 'react-router'

/**
 * La página que no existe, en el vocabulario del mundo: un cupón sin emitir.
 *
 * No lleva el sello VOID, y la distinción importa. El sello es para lo que
 * existió y se anuló —una reserva cancelada, un bloqueo vencido—; esto nunca
 * existió, así que lo que hay es un cupón en blanco con su troquel y nada
 * impreso dentro.
 */
export function NoEncontrada() {
  return (
    <section className="max-w-xl">
      <h1 className="titular mt-3 text-[clamp(2.25rem,7vw,3.5rem)]">Esta página no existe</h1>

      <p className="mt-4 max-w-[52ch] text-[0.9375rem] text-[var(--color-marina-media)]">
        No hay nada impreso en esta dirección. Si llegaste desde un enlace de una reserva, ábrelo
        de nuevo desde tu correo.
      </p>

      <div className="mt-8 flex flex-wrap gap-3">
        <Link to="/" className="boton no-underline">
          Volver al catálogo
        </Link>
        <Link to="/reservas" className="boton boton--secundario no-underline">
          Ver mis reservas
        </Link>
      </div>
    </section>
  )
}
