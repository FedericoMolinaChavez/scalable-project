// Package dominio contiene los tipos y reglas del negocio —reserva,
// disponibilidad, política de cancelación, voucher— sin dependencias de
// transporte ni de base de datos.
//
// La regla de solapamiento NO está aquí: la hace cumplir la restricción EXCLUDE
// de negocio.reserva. Este paquete define qué significa el error que devuelve
// el motor, no lo reimplementa.
package dominio
