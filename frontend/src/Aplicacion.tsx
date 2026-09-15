import { QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, NavLink, Route, Routes } from 'react-router'

import { crearClienteConsultas } from './api/clienteConsultas'
import { Catalogo } from './paginas/Catalogo'
import { MisReservas } from './paginas/MisReservas'
import { NoEncontrada } from './paginas/NoEncontrada'
import { Reservar } from './paginas/Reservar'

// Fuera del componente: creado dentro, se reconstruiría en cada render y
// vaciaría la caché entera sin avisar.
const clienteConsultas = crearClienteConsultas()

export function Aplicacion() {
  return (
    <QueryClientProvider client={clienteConsultas}>
      <BrowserRouter>
        <Estructura />
      </BrowserRouter>
    </QueryClientProvider>
  )
}

/**
 * La barra de compañía y el papel.
 *
 * La barra es azul de extremo a extremo, con el rótulo a la izquierda en
 * versalitas apretadas y la navegación en etiquetas pequeñas: es la cabecera de
 * la cartera de billetes, no un `header` con sombra. Debajo empieza el papel,
 * con su trama de seguridad.
 */
function Estructura() {
  return (
    <div className="flex min-h-screen flex-col">
      <header className="bg-[var(--color-marina)] text-white">
        <div className="mx-auto flex max-w-5xl flex-wrap items-center gap-x-6 gap-y-3 px-4 py-3 sm:px-6">
          <Rotulo />

          <nav className="flex items-center gap-5 sm:gap-6" aria-label="Principal">
            <Enlace a="/">Catálogo</Enlace>
            <Enlace a="/reservar">Reservar</Enlace>
            <Enlace a="/reservas">Mis reservas</Enlace>
          </nav>
        </div>
      </header>

      {/* flex-1: sin esto, una pantalla corta deja el pie a media altura con
          un vacío debajo, que se lee como una página que no terminó de
          cargar. */}
      <main className="mx-auto w-full max-w-5xl flex-1 px-4 py-8 sm:px-6 sm:py-12">
        <Routes>
          <Route path="/" element={<Catalogo />} />
          <Route path="/reservar" element={<Reservar />} />
          <Route path="/reservas" element={<MisReservas />} />
          <Route path="*" element={<NoEncontrada />} />
        </Routes>
      </main>

      <Pie />
    </div>
  )
}

/**
 * El rótulo.
 *
 * El icono está dibujado a mano y no tomado de una librería: son tres barras de
 * velocidad —el recurso, el tiempo y el tramo— con el mismo grosor de trazo que
 * el resto del sistema. Un glifo Unicode aquí sería un icono prestado de la
 * tipografía del sistema operativo, con otro peso y otro color.
 *
 * «Reservas» es una descripción, no una marca: PRODUCT.md dice explícitamente
 * que no existe nombre de producto ni identidad, así que la barra no inventa
 * ninguno.
 */
function Rotulo() {
  return (
    <div className="flex items-center gap-2.5">
      <svg width="26" height="16" viewBox="0 0 26 16" aria-hidden="true" className="shrink-0">
        <g fill="currentColor">
          <rect x="0" y="1" width="18" height="3" />
          <rect x="4" y="6.5" width="14" height="3" />
          <rect x="8" y="12" width="10" height="3" />
        </g>
        <g fill="var(--color-rojo)">
          <rect x="20" y="1" width="6" height="3" />
          <rect x="20" y="6.5" width="6" height="3" />
          <rect x="20" y="12" width="6" height="3" />
        </g>
      </svg>

      <span className="titular text-[1.0625rem] leading-none tracking-[-0.02em]">Reservas</span>
    </div>
  )
}

/**
 * Un enlace de la barra.
 *
 * El activo se marca en rojo y con un filete debajo, como la pestaña abierta de
 * un talonario. `aria-current` lo dice además sin depender del color, que es la
 * misma regla que gobierna los estados de una reserva.
 */
function Enlace({ a, children }: { a: string; children: string }) {
  return (
    <NavLink
      to={a}
      end={a === '/'}
      className={({ isActive }) =>
        [
          'border-b-2 pb-0.5 text-[0.6875rem] font-bold tracking-[0.13em] uppercase no-underline transition-colors',
          isActive
            ? 'border-[var(--color-rojo)] text-[var(--color-rojo)]'
            : 'border-transparent text-white/75 hover:text-white',
        ].join(' ')
      }
    >
      {children}
    </NavLink>
  )
}

/**
 * El pie: la letra pequeña del reverso del billete.
 *
 * Dice las dos cosas que una persona necesita saber y que ninguna pantalla
 * concreta es el sitio de repetir: que las horas son las del negocio (RF-38) y
 * que el instante final no le pertenece a la reserva.
 */
function Pie() {
  return (
    <footer className="mt-16 border-t border-[var(--color-filete)]">
      <div className="mx-auto max-w-5xl px-4 py-6 text-[0.6875rem] leading-relaxed text-[var(--color-marina-media)] sm:px-6">
        <p className="max-w-[65ch]">
          Todos los horarios se muestran en la zona del negocio, no en la de tu dispositivo. Un
          tramo se escribe «10:00 – 11:00» y el instante final no forma parte de él: la cita
          siguiente puede empezar justo a esa hora.
        </p>
      </div>
    </footer>
  )
}
