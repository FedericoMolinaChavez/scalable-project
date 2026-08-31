// Package reservas proyecta una fila de negocio.reserva sobre el modelo del
// contrato.
//
// Existe compartido porque lo usan dos componentes distintos de ARQ-01: el
// núcleo, que devuelve la reserva que acaba de crear, y el de consulta, que
// devuelve las que ya existen. Escrito dos veces, basta con que uno añada una
// columna y el otro no para que la misma reserva se vea distinta según qué
// servicio la sirva, y el cliente no tiene forma de saber cuál de los dos
// miente.
package reservas

import (
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
)

// Columnas es la proyección que Escanear espera, en su orden.
//
// El período se parte en lower/upper en vez de traerse el tstzrange entero: el
// contrato expone dos instantes, no un rango, y decodificar el tipo compuesto
// solo para volver a partirlo añade una conversión que puede fallar sin ganar
// nada.
const Columnas = `
	id::text, servicio_id::text, recurso_id::text,
	lower(periodo), upper(periodo), estado::text, expira_en,
	precio_cobrado::text, moneda, creada_en,
	contacto_nombre, contacto_email, contacto_telefono`

// Escanear lee una fila con la proyección de Columnas.
//
// Los UUID y los enum se leen como texto y se convierten aquí. Es una
// conversión de más frente a dejar que pgx los mapee, y compra que un valor que
// el motor acepte pero el contrato no —un estado nuevo en el enum de
// PostgreSQL que todavía no está en el OpenAPI— se vea en un sitio concreto en
// vez de propagarse como una cadena cualquiera.
//
// El monto viaja como texto de extremo a extremo. La columna es numeric, que es
// exacta; pasarlo por un float64 de Go introduciría el error de redondeo que el
// esquema y el contrato se han cuidado de evitar.
func Escanear(fila pgx.Row) (api.Reserva, error) {
	var (
		reserva                   api.Reserva
		id, servicioID, recursoID string
		estado                    string
		nombre, correo, telefono  *string
	)

	if err := fila.Scan(
		&id, &servicioID, &recursoID,
		&reserva.Periodo.Inicio, &reserva.Periodo.Fin, &estado, &reserva.ExpiraEn,
		&reserva.PrecioCobrado.Monto, &reserva.PrecioCobrado.Moneda, &reserva.CreadaEn,
		&nombre, &correo, &telefono,
	); err != nil {
		return api.Reserva{}, err
	}

	var err error
	if reserva.Id, err = uuid.Parse(id); err != nil {
		return api.Reserva{}, err
	}
	if reserva.ServicioId, err = uuid.Parse(servicioID); err != nil {
		return api.Reserva{}, err
	}
	if reserva.RecursoId, err = uuid.Parse(recursoID); err != nil {
		return api.Reserva{}, err
	}
	reserva.Estado = api.EstadoReserva(estado)

	// El contacto solo viaja cuando lo hay. Una reserva de cuenta autenticada
	// no lo lleva, y devolver un objeto con tres nulos dentro obligaría al
	// cliente a distinguir "sin contacto" de "contacto vacío".
	if nombre != nil || correo != nil {
		contacto := api.Contacto{Telefono: telefono}
		if nombre != nil {
			contacto.Nombre = *nombre
		}
		if correo != nil {
			// La columna es `text` y el contrato la tipa como Email. La
			// conversión es explícita porque el motor no valida el formato:
			// lo que sale de aquí es lo que entró, no una dirección
			// garantizada.
			contacto.Email = openapi_types.Email(*correo)
		}
		reserva.Contacto = &contacto
	}

	return reserva, nil
}
