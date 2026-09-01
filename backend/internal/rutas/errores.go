package rutas

import (
	"context"
	"errors"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/nucleo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// clasificar traduce un error de dentro a una clase de problema de fuera.
//
// Está en un solo sitio a propósito. Con la traducción repartida por los
// manejadores, el mismo error acaba siendo un 422 en una ruta y un 500 en otra
// según quién la escribiera, y la promesa de que "un error es indistinguible
// venga del componente que venga" se rompe sin que ninguna prueba lo note.
//
// El detalle que devuelve va hacia el cliente, así que sale del texto de los
// errores del dominio, que están escritos para leerse. Los errores internos NO
// llevan detalle: el mensaje de un fallo de pgx lleva dentro nombres de
// columnas y fragmentos de consulta.
func clasificar(err error) (transporte.Clase, string) {
	switch {
	// ------------------------------------------------------------- 404 --
	case errors.Is(err, datos.ErrNoEncontrado):
		return transporte.NoEncontrado, ""

	// ------------------------------------------------------------- 409 --
	// La invariante de RNF-10 rechazando un solapamiento. No es un fallo:
	// es haber perdido una carrera por el mismo cupo (RF-01, alt. 3).
	case errors.Is(err, datos.ErrHorarioOcupado):
		return transporte.HorarioOcupado,
			"Otra reserva se quedó con ese horario. Elige otra franja: reintentar la misma no la va a devolver."

	// ------------------------------------------------------------- 400 --
	case errors.Is(err, nucleo.ErrClaveIdempotenciaCorta),
		errors.Is(err, consulta.ErrCursorInvalido),
		errors.Is(err, disponibilidad.ErrRangoInvalido),
		errors.Is(err, dominio.ErrPeriodoInvalido):
		return transporte.Invalida, err.Error()

	// ------------------------------------------------------------- 422 --
	case errors.Is(err, dominio.ErrDuracionNoCoincide),
		errors.Is(err, dominio.ErrRecursoNoPresta),
		errors.Is(err, dominio.ErrFueraDeHorario),
		errors.Is(err, dominio.ErrPeriodoEnElPasado),
		errors.Is(err, nucleo.ErrSinPoliticaVigente),
		errors.Is(err, datos.ErrReferenciaInvalida),
		errors.Is(err, datos.ErrRestriccion):
		return transporte.ReglaNegocio, err.Error()

	case errors.Is(err, nucleo.ErrVoucherNoSoportado):
		return transporte.NoImplementado, err.Error()

	// ------------------------------------------------------------- 409 --
	// No es un fallo de la petición: la reserva simplemente ya no está en un
	// estado desde el que se pueda cancelar. Es un conflicto con el estado
	// actual del recurso, que es exactamente lo que significa un 409.
	case errors.Is(err, nucleo.ErrNoCancelable):
		return transporte.EstadoIncompatible, err.Error()

	// ------------------------------------------------------------- 422 --
	// Fuera de plazo es una regla del negocio, no un permiso: la respuesta
	// lleva cuántas horas hacían falta, porque "no se puede" a secas obliga a
	// adivinar por qué.
	case errors.Is(err, nucleo.ErrFueraDePlazo):
		return transporte.ReglaNegocio, err.Error()

	// ------------------------------------------------------------- 401 --
	case errors.Is(err, identidad.ErrTokenVencido):
		return transporte.TokenVencido, ""
	case errors.Is(err, identidad.ErrTokenInvalido), errors.Is(err, consulta.ErrSinAlcance):
		return transporte.NoAutorizado, ""

	// ------------------------------------------------------------- 500 --
	default:
		var excesiva disponibilidad.ErrVentanaExcesiva
		if errors.As(err, &excesiva) {
			return transporte.Invalida, excesiva.Error()
		}
		return transporte.Interno, ""
	}
}

// problema construye el cuerpo RFC 9457 de un error ya clasificado.
func problema(ctx context.Context, err error) (transporte.Clase, api.Problema) {
	clase, detalle := clasificar(err)
	return clase, clase.Cuerpo(ctx, detalle)
}

// invalida es el atajo para los errores que el manejador detecta por su cuenta,
// sin que venga ninguno de dentro.
func invalida(ctx context.Context, detalle string) api.Problema {
	return transporte.Invalida.Cuerpo(ctx, detalle)
}
