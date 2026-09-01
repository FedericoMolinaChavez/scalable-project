import { Link } from 'react-router'

export function NoEncontrada() {
  return (
    <section>
      <h1>Esta página no existe</h1>
      <Link to="/">Volver al catálogo</Link>
    </section>
  )
}
