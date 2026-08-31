import '@testing-library/jest-dom/vitest'

import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// Sin esto, el DOM de una prueba sobrevive a la siguiente y las consultas por
// texto encuentran nodos de la prueba anterior: fallos que dependen del orden
// de ejecución y desaparecen al correr el archivo suelto.
afterEach(cleanup)
