// Package pagos es el componente "Webhook de Pagos" de ARQ-01, en sus dos
// direcciones.
//
// ARQ-01 lo dibuja como una caja aparte del Núcleo de Reservas y con una nota
// que explica por qué: su disponibilidad la acota Stripe, así que vive fuera
// del presupuesto de RNF-04 y por eso la reserva se confirma AQUÍ y no en la
// ruta síncrona. La regla de descomposición es la misma que separa todo lo
// demás —frontera transaccional y dominio de fallo— y el pago la cumple por los
// dos lados: depende de un tercero y tolera latencia.
//
// De ahí salen las dos rutas que sirve:
//
//	POST /v1/pagos/intencion      hacia Stripe: abre el cobro
//	POST /v1/webhooks/stripe      desde Stripe: lo confirma
//
// Y una tercera que existe solo por la asimetría entre ambas: cuando el
// navegador recibe "succeeded" de Stripe, la reserva de este sistema todavía
// está `pendiente` durante unos cientos de milisegundos, hasta que el webhook
// llegue. GET /v1/pagos/{id}/estado es cómo la interfaz sabe cuándo dejar de
// esperar.
//
// Lo que este paquete NO hace: cobrar. El importe no pasa por aquí y la tarjeta
// tampoco (RNF-05). Lo único que sale hacia el navegador es un `client_secret`,
// que sirve para confirmar ESE cobro y nada más.
package pagos

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

var (
	// ErrReservaNoPagable: la reserva existe pero ya no admite pago. Venció su
	// bloqueo, ya está confirmada, o se canceló. No es un error de la petición:
	// es que llegó tarde.
	ErrReservaNoPagable = errors.New("esta reserva ya no admite pago")

	// ErrSinComprobante: todavía no se emitió, o la reserva no es de quien
	// pregunta. Los dos casos responden igual por la razón de siempre.
	ErrSinComprobante = errors.New("no hay comprobante para esa reserva")
)

// Almacen es lo que el componente necesita del almacenamiento de objetos: firmar
// el enlace de descarga de un comprobante. Es una interfaz para que el binario
// de consulta pueda montar la ruta sin arrastrar el cliente de MinIO a sus
// pruebas.
type Almacen interface {
	EnlaceFirmado(ctx context.Context, clave string, vigencia time.Duration) (string, time.Time, error)
}

// VigenciaEnlace es cuánto vale una URL firmada de comprobante.
//
// Quince minutos, y es corto a propósito: el enlace ES la credencial. Uno que
// dura un día es un documento que se reenvía por chat y sigue abriéndose
// mañana, para cualquiera que reciba el mensaje.
const VigenciaEnlace = 15 * time.Minute

// Servicio resuelve la intención de pago y el seguimiento de la confirmación.
type Servicio struct {
	bd       *datos.BD
	pasarela Pasarela
	almacen  Almacen
	registro *slog.Logger
}

func Nuevo(bd *datos.BD, pasarela Pasarela, almacen Almacen, registro *slog.Logger) *Servicio {
	return &Servicio{bd: bd, pasarela: pasarela, almacen: almacen, registro: registro}
}

// NuevoLector construye la mitad que solo lee: el comprobante de RF-34.
//
// Existe porque esa ruta la sirve `consulta` y no `pagos`, y la diferencia es
// la de siempre en ARQ-01: leer un comprobante ya emitido es una lectura, va
// contra las réplicas, y no necesita hablar con Stripe. Construir la pasarela
// aquí obligaría al binario de lectura a llevar la clave secreta del proveedor
// para no usarla nunca, que es una credencial repartida a cambio de nada.
//
// Sin pasarela, llamar a Intencion o al webhook desde este servicio sería un
// pánico. No puede ocurrir: Montar registra esas rutas bajo un componente
// distinto, y ese componente es el que sí la lleva.
func NuevoLector(bd *datos.BD, almacen Almacen, registro *slog.Logger) *Servicio {
	return &Servicio{bd: bd, almacen: almacen, registro: registro}
}

// ClavePublicable la sirve la respuesta de la intención. Ver el contrato.
func (s *Servicio) ClavePublicable() string { return s.pasarela.ClavePublicable() }

// ---------------------------------------------------------------- intención --

// Intento es lo que la interfaz necesita para cobrar.
type Intento struct {
	ClientSecret string
	Monto        Monto
	Estado       Estado
	ExpiraEn     *time.Time
}

// Intencion abre —o recupera— el cobro de una reserva pendiente.
//
// El orden de las tres partes es lo que importa aquí, y no es el obvio:
//
//  1. Se lee la reserva y se comprueba que todavía admite pago.
//  2. Se llama a Stripe, FUERA de cualquier transacción.
//  3. Se escribe la fila del pago.
//
// Llamar a Stripe dentro de la transacción sería lo cómodo y está mal por dos
// razones distintas. La primera es que mantendría abierta una transacción
// durante una ida y vuelta a Internet, reteniendo una conexión de PgBouncer y
// el horizonte del vacuum. La segunda es peor: si el COMMIT falla después, el
// PaymentIntent ya existe en Stripe y nadie tiene su identificador, así que
// queda un cobro posible del que este sistema no sabe nada.
//
// Con este orden, el peor caso es un PaymentIntent creado y no registrado, que
// el conciliador de RF-33 acaba encontrando por el otro lado —preguntándole a
// Stripe— y el cliente no puede pagar porque nunca recibió el secreto.
func (s *Servicio) Intencion(
	ctx context.Context, tenant, reserva uuid.UUID,
) (Intento, error) {
	datosReserva, err := s.leerParaPago(ctx, tenant, reserva)
	if err != nil {
		return Intento{}, err
	}

	// Camino idempotente: ya hay un intento vivo. Se devuelve ese y no se crea
	// otro. Sin esto, dos pestañas abiertas sobre la misma reserva producirían
	// dos PaymentIntent y se podrían pagar los dos: la restricción EXCLUDE
	// protege el cupo, pero nada protegería el dinero.
	if datosReserva.intencionVigente != "" {
		intencion, err := s.pasarela.ConsultarIntencion(ctx, datosReserva.intencionVigente)
		if err == nil {
			return Intento{
				ClientSecret: intencion.ClientSecret,
				Monto:        datosReserva.monto,
				Estado:       intencion.Estado,
				ExpiraEn:     datosReserva.expiraEn,
			}, nil
		}

		// El proveedor no conoce esa intención: nuestra fila apunta a algo que
		// no existe. Se marca fallida y se sigue creando una nueva, que es la
		// única salida que deja a la persona pagar. Cualquier otro error del
		// proveedor sí se propaga: reintentar contra un Stripe caído creando
		// intenciones nuevas multiplicaría los cobros posibles.
		if !errors.Is(err, ErrIntencionNoEncontrada) {
			return Intento{}, err
		}
		if err := s.marcarFallido(ctx, tenant, datosReserva.intencionVigente,
			"el proveedor no reconoce esta intención de pago"); err != nil {
			return Intento{}, err
		}
	}

	intencion, err := s.pasarela.CrearIntencion(ctx, Solicitud{
		Monto:   datosReserva.monto,
		Tenant:  tenant.String(),
		Reserva: reserva.String(),
		Correo:  datosReserva.correo,

		// La clave se deriva de la reserva y no se sortea. Si esta petición se
		// reintenta tras un timeout, Stripe devuelve el PaymentIntent que ya
		// creó en vez de crear un segundo: es la misma garantía que
		// Idempotency-Key da al crear la reserva, un escalón más abajo.
		ClaveIdempotencia: "reserva:" + reserva.String(),
		Descripcion:       datosReserva.descripcion,
	})
	if err != nil {
		return Intento{}, err
	}

	if err := s.registrarIntencion(ctx, tenant, reserva, intencion, datosReserva.monto); err != nil {
		return Intento{}, err
	}

	return Intento{
		ClientSecret: intencion.ClientSecret,
		Monto:        datosReserva.monto,
		Estado:       intencion.Estado,
		ExpiraEn:     datosReserva.expiraEn,
	}, nil
}

// paraPago es lo que hace falta saber de la reserva antes de cobrarla.
type paraPago struct {
	monto            Monto
	correo           string
	descripcion      string
	expiraEn         *time.Time
	intencionVigente string
}

func (s *Servicio) leerParaPago(
	ctx context.Context, tenant, reserva uuid.UUID,
) (paraPago, error) {
	var (
		resultado paraPago
		monto     string
		moneda    string
		estado    string
		correo    *string
		servicio  string
		intencion *string
		expira    *time.Time
	)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.precio_cobrado::text, r.moneda, r.estado::text, r.expira_en,
			       r.contacto_email, s.nombre,
			       (SELECT p.payment_intent_id
			          FROM negocio.pago p
			         WHERE p.reserva_id = r.id AND p.estado = 'iniciado')
			FROM negocio.reserva r
			JOIN negocio.servicio s
			  ON s.tenant_id = r.tenant_id AND s.id = r.servicio_id
			WHERE r.id = $1`, reserva,
		).Scan(&monto, &moneda, &estado, &expira, &correo, &servicio, &intencion)
	})
	if err != nil {
		return paraPago{}, err
	}

	// Solo una reserva pendiente y todavía viva se puede pagar. Una confirmada
	// ya está cobrada; una expirada perdió el cupo, y cobrar por ella sería
	// vender algo que ya no se tiene.
	if estado != "pendiente" {
		return paraPago{}, fmt.Errorf("%w: está %s", ErrReservaNoPagable, estado)
	}
	if expira != nil && !expira.After(time.Now()) {
		return paraPago{}, fmt.Errorf("%w: el bloqueo venció", ErrReservaNoPagable)
	}

	if resultado.monto, err = DesdeTexto(monto, moneda); err != nil {
		return paraPago{}, err
	}
	if correo != nil {
		resultado.correo = *correo
	}
	if intencion != nil {
		resultado.intencionVigente = *intencion
	}
	resultado.expiraEn = expira
	resultado.descripcion = servicio

	return resultado, nil
}

func (s *Servicio) registrarIntencion(
	ctx context.Context, tenant, reserva uuid.UUID, intencion Intencion, monto Monto,
) error {
	return s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO negocio.pago
				(tenant_id, reserva_id, proveedor, payment_intent_id, monto, moneda, estado)
			VALUES ($1, $2, 'stripe', $3, $4::numeric, $5, 'iniciado')
			ON CONFLICT (tenant_id, payment_intent_id) DO NOTHING`,
			tenant, reserva, intencion.ID, monto.Texto(), monto.Moneda)
		return err
	})
}

func (s *Servicio) marcarFallido(
	ctx context.Context, tenant uuid.UUID, intencionID, motivo string,
) error {
	return s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE negocio.pago
			SET estado = 'fallido', motivo_fallo = $2
			WHERE payment_intent_id = $1 AND estado = 'iniciado'`,
			intencionID, motivo)
		return err
	})
}

// ------------------------------------------------------------ seguimiento --

// Confirmacion es dónde está la confirmación asíncrona de RF-33.
type Confirmacion struct {
	ReservaEstado string
	PagoEstado    string
	MotivoFallo   string
}

// Estado de la confirmación de una reserva.
//
// Deliberadamente pobre: el estado de la reserva, el del pago y el motivo del
// último rechazo. Es pública, así que no puede decir nada más —ni el contacto,
// ni el importe, ni el horario— porque quien pregunta solo ha demostrado
// conocer un identificador.
func (s *Servicio) Estado(
	ctx context.Context, tenant, reserva uuid.UUID,
) (Confirmacion, error) {
	var (
		confirmacion Confirmacion
		pagoEstado   *string
		motivo       *string
	)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT r.estado::text, p.estado::text, p.motivo_fallo
			FROM negocio.reserva r
			LEFT JOIN LATERAL (
				SELECT estado, motivo_fallo
				FROM negocio.pago
				WHERE reserva_id = r.id
				ORDER BY creado_en DESC
				LIMIT 1
			) p ON true
			WHERE r.id = $1`, reserva,
		).Scan(&confirmacion.ReservaEstado, &pagoEstado, &motivo)
	})
	if err != nil {
		return Confirmacion{}, err
	}

	if pagoEstado != nil {
		confirmacion.PagoEstado = *pagoEstado
	}
	// El motivo solo acompaña a un pago que de verdad falló. Enseñar el último
	// rechazo de una tarjeta junto a un pago ya confirmado diría que algo salió
	// mal cuando salió bien.
	if motivo != nil && confirmacion.PagoEstado != string(Confirmado) {
		confirmacion.MotivoFallo = *motivo
	}

	return confirmacion, nil
}

// ----------------------------------------------------------- comprobante --

// Documento es el comprobante de RF-34 tal como sale hacia el cliente.
type Documento struct {
	Numero      string
	Tipo        string
	EmitidoEn   time.Time
	URL         string
	URLExpiraEn *time.Time
}

// Comprobante devuelve el comprobante de una reserva, con el enlace firmado.
//
// `destino` es el correo que el token acreditó, y va en el WHERE por la misma
// razón que en la cancelación: una reserva ajena devuelve cero filas, igual que
// una que no existe. Un comprobante lleva nombre, correo e importe —es el
// documento con más datos personales del flujo— y no se entrega por conocer un
// identificador.
func (s *Servicio) Comprobante(
	ctx context.Context, tenant, reserva uuid.UUID, destino string,
) (Documento, error) {
	var (
		doc   Documento
		clave *string
	)

	err := s.bd.EnTenant(ctx, tenant.String(), func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT c.numero, c.tipo::text, c.emitido_en, c.objeto_clave
			FROM negocio.comprobante c
			JOIN negocio.reserva r
			  ON r.tenant_id = c.tenant_id AND r.id = c.reserva_id
			WHERE c.reserva_id = $1
			  AND r.cuenta_id IS NULL
			  AND lower(r.contacto_email) = $2
			ORDER BY c.emitido_en DESC
			LIMIT 1`, reserva, destino,
		).Scan(&doc.Numero, &doc.Tipo, &doc.EmitidoEn, &clave)
	})
	if errors.Is(err, datos.ErrNoEncontrado) {
		return Documento{}, ErrSinComprobante
	}
	if err != nil {
		return Documento{}, err
	}

	// El documento puede no estar todavía: la fila se emite dentro de la
	// transacción y el objeto se sube después. La ventana es de segundos, y en
	// ella el comprobante existe con su número y sin enlace, que es exactamente
	// lo que el contrato declara.
	if clave == nil || s.almacen == nil {
		return doc, nil
	}

	enlace, expira, err := s.almacen.EnlaceFirmado(ctx, *clave, VigenciaEnlace)
	if err != nil {
		// No se propaga: el comprobante existe y su número es información
		// válida. Fallar entero porque no se pudo firmar el enlace convertiría
		// una degradación del almacén en un 500 sobre datos que sí están.
		s.registro.WarnContext(ctx, "no se pudo firmar el enlace del comprobante",
			slog.String("comprobante", doc.Numero), slog.String("error", err.Error()))
		return doc, nil
	}

	doc.URL = enlace
	doc.URLExpiraEn = &expira
	return doc, nil
}
