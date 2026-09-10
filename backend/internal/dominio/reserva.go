package dominio

import (
	"errors"
	"time"
)

// Las reglas que el motor NO hace cumplir.
//
// La lista es corta a propósito, y lo que no está en ella importa tanto como
// lo que está: el solapamiento no aparece aquí porque lo rechaza la restricción
// EXCLUDE, y reimplementarlo en Go crearía una segunda verdad que se
// desincronizaría con la primera. Aquí solo vive lo que ninguna restricción del
// esquema puede expresar, porque exige mirar varias tablas a la vez.
var (
	// ErrPeriodoInvalido: un período vacío o invertido. El esquema lo rechaza
	// con reserva_periodo_valido, pero llega antes aquí para poder señalar el
	// campo concreto en la respuesta en vez de traducir un 23514 opaco.
	ErrPeriodoInvalido = errors.New("el período debe ser [inicio, fin) con fin posterior a inicio")

	// ErrPeriodoEnElPasado: la cita empieza antes de ahora. El motor lo acepta
	// sin protestar, y hace bien: un administrador tendrá que registrar
	// asistencias a posteriori (RF-32). Lo que no puede es ocurrir por la ruta
	// del cliente, donde reservar ayer no significa nada.
	ErrPeriodoEnElPasado = errors.New("la cita empieza en el pasado")

	// ErrDuracionNoCoincide: el período no dura lo que dura el servicio. Sin
	// esta comprobación, un cliente podría pedir ocho horas de un servicio de
	// una y bloquear el recurso el día entero pagando el precio de una hora.
	ErrDuracionNoCoincide = errors.New("el período no dura lo que dura el servicio")

	// ErrRecursoNoPresta: el recurso existe, pero no está asociado al servicio.
	// El motor solo comprueba que ambos existan en el tenant; que uno preste el
	// otro vive en servicio_recurso y hay que consultarlo.
	ErrRecursoNoPresta = errors.New("el recurso no presta ese servicio")

	// ErrFueraDeHorario: el período no cae dentro de ninguna regla de
	// disponibilidad vigente del recurso, o lo tapa una excepción de
	// calendario. La restricción EXCLUDE no lo cubre —solo impide dos reservas
	// solapadas—, así que sin esto se podría reservar a las tres de la mañana
	// de un domingo cerrado.
	ErrFueraDeHorario = errors.New("el período cae fuera del horario disponible del recurso")
)

// Periodo es el intervalo semiabierto [Inicio, Fin).
//
// El instante final NO pertenece al período, y de ahí depende que dos citas
// consecutivas quepan: con ambos límites cerrados, 10:00–11:00 y 11:00–12:00
// compartirían el instante 11:00, el operador && de PostgreSQL las declararía
// solapadas y la restricción EXCLUDE rechazaría dos reservas perfectamente
// válidas.
type Periodo struct {
	Inicio time.Time
	Fin    time.Time
}

// Valido comprueba que el período no esté vacío ni invertido.
func (p Periodo) Valido() bool {
	return p.Fin.After(p.Inicio)
}

// Duracion es cuánto ocupa el recurso.
func (p Periodo) Duracion() time.Duration {
	return p.Fin.Sub(p.Inicio)
}

// CoincideCon indica si el período dura exactamente los minutos del servicio.
//
// Exactamente, no "al menos": una reserva más corta que el servicio dejaría un
// hueco que la rejilla de disponibilidad no sabe ofrecer a nadie, y una más
// larga ocuparía tiempo que no se ha cobrado.
func (p Periodo) CoincideCon(duracionMin int) bool {
	return p.Duracion() == time.Duration(duracionMin)*time.Minute
}

// ErrPuntajeInvalido es el rango de RF-20: de 1 a 5.
//
// Está aquí y no en el paquete que lo usa porque es una regla sobre un valor,
// no sobre una fila: el mismo rango lo comprueban el contrato al recibirlo, el
// CHECK del esquema al guardarlo y esta constante en medio.
var ErrPuntajeInvalido = errors.New("el puntaje tiene que estar entre 1 y 5")
