package dominio

import "errors"

// Alcance es RF-23 hecho un tipo: de quién son los datos que una petición puede
// tocar.
//
// Vive aquí y no en cada componente porque lo usan los dos lados de la frontera
// transaccional de ARQ-01 —consulta al leer, núcleo al cancelar— y una misma
// petición no puede significar una cosa leyendo y otra escribiendo. Con una
// copia por paquete, añadir un caso a uno y no al otro es un fallo que compila.
//
// Los tres campos no son excluyentes por construcción y no deben serlo: una
// cuenta lleva Cuenta y además Destino, porque su alcance incluye lo que hizo
// como invitado con ese mismo correo antes de registrarse (RF-24).
type Alcance struct {
	// Cuenta es el identificador de la cuenta que pide (RF-12). Vacío en un
	// invitado, que no tiene ninguna.
	Cuenta string

	// Destino es el correo VERIFICADO. En un invitado es todo su alcance: las
	// reservas hechas con esa dirección (RF-02). En una cuenta es el añadido de
	// RF-24, y por eso solo viaja cuando el correo está verificado: si bastara
	// con escribirlo en el perfil, poner la dirección de otra persona sería
	// suficiente para heredar sus reservas.
	Destino string

	// TenantCompleto es el alcance del administrador: todo lo de su tenant
	// (RF-32). No hace falta decir cuál, porque la transacción ya corre con su
	// tenant fijado y RLS no deja ver otro.
	TenantCompleto bool
}

// Vacio dice si este alcance no acota nada.
//
// Se pregunta antes de consultar y falla cerrado. El error posible es no
// devolver nada; el inaceptable sería devolver los datos de otra persona porque
// alguien se olvidó de rellenar el alcance.
func (a Alcance) Vacio() bool {
	return !a.TenantCompleto && a.Cuenta == "" && a.Destino == ""
}

// ErrSinAlcance se devuelve cuando se opera sobre reservas sin decir de quién
// son.
//
// Falla cerrado a propósito, y por eso es un error y no un alcance por defecto:
// el error posible es no devolver nada; el inaceptable sería que un fallo de
// cableado —una ruta montada sin el middleware que verifica el token— hiciera
// que la consulta devolviera las reservas de todo el mundo.
var ErrSinAlcance = errors.New("no se puede operar sobre reservas sin saber de quién son")
