import { QueryClientProvider } from '@tanstack/react-query'
import { BrowserRouter, Link, Route, Routes } from 'react-router'

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

function Estructura() {
  return (
    <div className="min-h-screen">
      <header className="border-b">
        <nav className="flex gap-4 p-4">
          <Link to="/">Catálogo</Link>
          <Link to="/reservar">Reservar</Link>
          {/* No es «Mis reservas»: sin RF-12 la ruta devuelve las del tenant
              entero, que es el alcance del administrador (RF-32). */}
          <Link to="/reservas">Reservas del negocio</Link>
        </nav>
      </header>

      <main className="p-4">
        <Routes>
          <Route path="/" element={<Catalogo />} />
          <Route path="/reservar" element={<Reservar />} />
          <Route path="/reservas" element={<MisReservas />} />
          <Route path="*" element={<NoEncontrada />} />
        </Routes>
      </main>
    </div>
  )
}
