import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'

import { Aplicacion } from './Aplicacion'
import './index.css'

const raiz = document.getElementById('raiz')
if (!raiz) {
  throw new Error('no se encontró el elemento #raiz en index.html')
}

createRoot(raiz).render(
  <StrictMode>
    <Aplicacion />
  </StrictMode>,
)
