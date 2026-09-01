import { cabeceras, cliente, ErrorApi } from './cliente'

/**
 * Identificación del cliente por código de un solo uso (RF-02).
 *
 * No es inicio de sesión. Quien reservó como invitado no tiene cuenta: lo único
 * que se demuestra aquí es que controla el correo con el que reservó, y eso da
 * acceso exactamente a las reservas hechas con esa dirección.
 */

/**
 * El token vive en `sessionStorage`, no en `localStorage` ni en una cookie.
 *
 * Frente a localStorage: el token caduca en minutos y no se puede revocar
 * —un invitado no tiene sesión que gestionar—, así que sobrevivir al cierre de
 * la pestaña solo alarga la ventana en la que sirve de algo a quien lo robe.
 *
 * Frente a una cookie: una cookie viajaría sola en cada petición al origen, y
 * este token solo debe ir a las rutas que lo piden. Mandarlo a todas es
 * regalarlo al catálogo, que es público.
 *
 * Es un compromiso consciente, no la opción segura: cualquier script que se
 * cuele en la página lo puede leer. La defensa real sigue siendo que caduca
 * pronto.
 */
const CLAVE = 'reservas.token'

export type Sesion = {
  token: string
  expiraEn: string
}

export function sesionGuardada(): Sesion | null {
  const crudo = globalThis.sessionStorage?.getItem(CLAVE)
  if (!crudo) return null

  try {
    const sesion = JSON.parse(crudo) as Sesion
    // Se descarta al leerla, no al usarla. Enseñar la lista y que falle a
    // continuación es peor que pedir el código de entrada.
    if (!sesion.token || new Date(sesion.expiraEn) <= new Date()) {
      olvidarSesion()
      return null
    }
    return sesion
  } catch {
    olvidarSesion()
    return null
  }
}

function guardarSesion(sesion: Sesion): void {
  globalThis.sessionStorage?.setItem(CLAVE, JSON.stringify(sesion))
}

export function olvidarSesion(): void {
  globalThis.sessionStorage?.removeItem(CLAVE)
}

/** Cabecera de autorización, o nada si no hay sesión. */
export function autorizacion(): Record<string, string> {
  const sesion = sesionGuardada()
  return sesion ? { Authorization: `Bearer ${sesion.token}` } : {}
}

/**
 * Pide el código. La respuesta es la misma exista o no el correo (RF-12 A12),
 * así que esta función no puede decir si esa dirección tiene reservas — y la
 * interfaz tampoco debe insinuarlo.
 */
export async function solicitarCodigo(destino: string): Promise<void> {
  const { error, response } = await cliente.POST('/v1/sesiones/codigo', {
    params: { header: cabeceras },
    body: { destino },
  })

  if (error) throw new ErrorApi(response.status, error)
}

/** Canjea el código por el token de acceso y lo guarda. */
export async function canjearCodigo(destino: string, codigo: string): Promise<Sesion> {
  const { data, error, response } = await cliente.POST('/v1/sesiones/token', {
    params: { header: cabeceras },
    body: { destino, codigo },
  })

  if (error) throw new ErrorApi(response.status, error)

  const sesion = { token: data.token, expiraEn: data.expira_en }
  guardarSesion(sesion)
  return sesion
}
