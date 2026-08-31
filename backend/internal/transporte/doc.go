// Package transporte contiene lo compartido por la capa HTTP: middleware
// (identificador de petición, registro, recuperación de pánico, tiempo límite),
// el formato único de error de la API y los ayudantes de codificación.
//
// Los manejadores concretos viven junto a su servicio; aquí solo está lo que
// todos los servicios deben hacer igual para que una respuesta de error sea
// indistinguible venga del componente que venga.
package transporte
