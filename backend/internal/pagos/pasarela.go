package pagos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/stripe/stripe-go/v86"
	"github.com/stripe/stripe-go/v86/webhook"
)

// La frontera con Stripe, y por qué es una interfaz.
//
// Todo lo que este sistema hace con dinero pasa por aquí, y nada de lo que hay
// detrás se puede ejercitar en una prueba: crear un PaymentIntent es una
// llamada de red a un tercero con estado propio. La interfaz permite que el
// núcleo de esta lógica —cuándo se cobra, cuándo se devuelve, qué pasa cuando
// llega un evento de una reserva que ya no existe— se pruebe sin red.
//
// No es una abstracción "por si cambiamos de proveedor". ER-03 declara el enum
// del proveedor con un solo valor y así se queda; lo que compra esta interfaz
// es poder probar, que es un motivo mucho más concreto.

// Pasarela es lo que el componente de pagos necesita del proveedor.
type Pasarela interface {
	// CrearIntencion abre un intento de cobro. La metadata viaja con él y es
	// lo que permite que el webhook sepa de qué tenant y de qué reserva habla
	// un evento que llega sin contexto.
	CrearIntencion(ctx context.Context, sol Solicitud) (Intencion, error)

	// ConsultarIntencion pregunta por el estado actual. Es la mitad de la
	// reconciliación de RF-33: cuando el webhook no llega, se pregunta.
	ConsultarIntencion(ctx context.Context, id string) (Intencion, error)

	// CancelarIntencion cierra un intento que ya no tiene sentido, porque el
	// bloqueo venció. Sin esto, un PaymentIntent abandonado sigue siendo
	// cobrable durante días desde una pestaña que nadie cerró.
	CancelarIntencion(ctx context.Context, id string) error

	// Reembolsar devuelve el dinero de un cobro (RF-29).
	Reembolsar(ctx context.Context, intencionID string, monto Monto, motivo string) (Reembolso, error)

	// ClavePublicable es la que inicializa Stripe.js en el navegador. Se sirve
	// desde la API y no desde una variable de compilación del frontend: es del
	// entorno, no del artefacto.
	ClavePublicable() string

	// VerificarEvento comprueba la firma sobre los bytes EXACTOS del cuerpo.
	// Recibe []byte y no una estructura por eso mismo: cualquier
	// decodificación previa cambiaría los bytes y la firma dejaría de cuadrar.
	VerificarEvento(cuerpo []byte, firma string) (Evento, error)
}

// Solicitud es lo que se le pide al proveedor para abrir un cobro.
type Solicitud struct {
	Monto  Monto
	Tenant string
	// Reserva viaja en la metadata y vuelve en cada evento. Es lo que hace que
	// el webhook pueda resolver a qué reserva pertenece un cobro sin consultar
	// nada: la relación va con el objeto de Stripe, no en una tabla que
	// habría que buscar antes de saber en qué tenant buscar.
	Reserva string
	Correo  string
	// ClaveIdempotencia se la pasamos a Stripe tal cual. Su semántica es la
	// misma que la nuestra: repetir la llamada con la misma clave devuelve el
	// objeto que ya se creó en vez de crear otro.
	ClaveIdempotencia string
	Descripcion       string
}

// Intencion es la vista propia de un PaymentIntent. Solo lo que se usa.
type Intencion struct {
	ID           string
	ClientSecret string
	Estado       Estado
	MotivoFallo  string
}

// Reembolso es la vista propia de un Refund.
type Reembolso struct {
	ID     string
	Estado EstadoReembolso
	Error  string
}

// Evento es un webhook ya verificado.
type Evento struct {
	ID   string
	Tipo string

	// Datos es el objeto que el evento transporta, ya extraído. Solo se
	// interpretan los tipos que este componente atiende; el resto llega con
	// Datos vacío y se registra igual, porque la bitácora de eventos vale
	// también para lo que todavía no se procesa.
	Intencion Intencion
	Tenant    string
	Reserva   string

	// Crudo es el cuerpo original, que se guarda en la bitácora. Sirve para
	// reprocesar un evento cuyo manejo se corrigió después.
	Crudo []byte
}

// Estado del intento de cobro, en el vocabulario de ER-03 y no en el de Stripe.
type Estado string

const (
	Iniciado   Estado = "iniciado"
	Confirmado Estado = "confirmado"
	Fallido    Estado = "fallido"
)

// EstadoReembolso es el de ER-03, igualmente traducido.
type EstadoReembolso string

const (
	ReembolsoPendiente  EstadoReembolso = "pendiente"
	ReembolsoConfirmado EstadoReembolso = "confirmado"
	ReembolsoFallido    EstadoReembolso = "fallido"
)

// ErrProveedor envuelve todo fallo que venga de Stripe.
//
// Existe para que la capa HTTP pueda responder 502 y no 500. La distinción no
// es cosmética: dice de qué lado está el problema, y con ella una alerta puede
// separar "nuestro sistema falla" de "Stripe está caído", que son dos
// incidentes con dos respuestas distintas.
var ErrProveedor = errors.New("el proveedor de pago no pudo atender la operación")

// ErrFirmaInvalida: el evento no viene de Stripe, o no viene entero.
var ErrFirmaInvalida = errors.New("la firma del evento no es válida")

// ErrIntencionNoEncontrada: el PaymentIntent no existe en el proveedor.
var ErrIntencionNoEncontrada = errors.New("el proveedor no conoce esa intención de pago")

// ---------------------------------------------------------------- Stripe --

// StripeConfig son las tres credenciales que hacen falta.
type StripeConfig struct {
	ClaveSecreta    string
	ClavePublicable string

	// SecretoWebhook firma los eventos entrantes. Sin él, cualquiera que
	// conozca la URL del webhook puede confirmar reservas que nadie pagó, así
	// que el binario se niega a arrancar si falta.
	SecretoWebhook string
}

type stripePasarela struct {
	cliente *stripe.Client
	cfg     StripeConfig
}

// NuevaStripe construye la pasarela real.
func NuevaStripe(cfg StripeConfig) Pasarela {
	return &stripePasarela{cliente: stripe.NewClient(cfg.ClaveSecreta), cfg: cfg}
}

func (s *stripePasarela) ClavePublicable() string { return s.cfg.ClavePublicable }

func (s *stripePasarela) CrearIntencion(ctx context.Context, sol Solicitud) (Intencion, error) {
	params := &stripe.PaymentIntentCreateParams{
		Amount:   stripe.Int64(sol.Monto.Menor()),
		Currency: stripe.String(strings.ToLower(sol.Monto.Moneda)),

		// Los métodos automáticos dejan que Stripe decida qué ofrecer según el
		// país y la moneda del negocio, que es lo que hace que esto siga
		// funcionando cuando un tenant nuevo cobre en otra divisa (RF-38). La
		// alternativa —enumerar "card" a mano— habría que ampliarla cada vez.
		AutomaticPaymentMethods: &stripe.PaymentIntentCreateAutomaticPaymentMethodsParams{
			Enabled: stripe.Bool(true),

			// Sin redirecciones. La interfaz cobra dentro de la página, bajo el
			// reloj de RF-27; un método que se lleva a la persona a otro sitio
			// y la devuelve rompe justo lo que el temporizador hace visible.
			AllowRedirects: stripe.String(
				string(stripe.PaymentIntentAutomaticPaymentMethodsAllowRedirectsNever)),
		},

		// La metadata es el puente entre los dos mundos. Un evento de Stripe
		// llega sin saber nada de tenants; con esto, el webhook resuelve a qué
		// fila pertenece sin buscar en 64 particiones de cada negocio.
		Metadata: map[string]string{
			metadatoTenant:  sol.Tenant,
			metadatoReserva: sol.Reserva,
		},

		Description: stripe.String(sol.Descripcion),
	}

	if sol.Correo != "" {
		params.ReceiptEmail = stripe.String(sol.Correo)
	}
	if sol.ClaveIdempotencia != "" {
		params.SetIdempotencyKey(sol.ClaveIdempotencia)
	}

	intencion, err := s.cliente.V1PaymentIntents.Create(ctx, params)
	if err != nil {
		return Intencion{}, fmt.Errorf("%w: crear intención: %w", ErrProveedor, err)
	}

	return traducirIntencion(intencion), nil
}

func (s *stripePasarela) ConsultarIntencion(ctx context.Context, id string) (Intencion, error) {
	intencion, err := s.cliente.V1PaymentIntents.Retrieve(ctx, id, nil)
	if err != nil {
		if esNoEncontrado(err) {
			return Intencion{}, fmt.Errorf("%w: %s", ErrIntencionNoEncontrada, id)
		}
		return Intencion{}, fmt.Errorf("%w: consultar intención: %w", ErrProveedor, err)
	}
	return traducirIntencion(intencion), nil
}

func (s *stripePasarela) CancelarIntencion(ctx context.Context, id string) error {
	_, err := s.cliente.V1PaymentIntents.Cancel(ctx, id, nil)
	if err != nil {
		if esNoEncontrado(err) {
			return fmt.Errorf("%w: %s", ErrIntencionNoEncontrada, id)
		}
		return fmt.Errorf("%w: cancelar intención: %w", ErrProveedor, err)
	}
	return nil
}

func (s *stripePasarela) Reembolsar(
	ctx context.Context, intencionID string, monto Monto, motivo string,
) (Reembolso, error) {
	params := &stripe.RefundCreateParams{
		PaymentIntent: stripe.String(intencionID),
		Amount:        stripe.Int64(monto.Menor()),
		Metadata:      map[string]string{"motivo": motivo},
	}

	// La clave de idempotencia es el propio PaymentIntent, y eso basta porque
	// el esquema ya impide más de un reembolso por pago: si esta llamada se
	// repite tras un fallo de red, Stripe devuelve el reembolso que ya creó en
	// vez de devolver el dinero dos veces.
	params.SetIdempotencyKey("reembolso:" + intencionID)

	reembolso, err := s.cliente.V1Refunds.Create(ctx, params)
	if err != nil {
		return Reembolso{}, fmt.Errorf("%w: reembolsar: %w", ErrProveedor, err)
	}

	return Reembolso{
		ID:     reembolso.ID,
		Estado: traducirEstadoReembolso(reembolso.Status),
		Error:  razonDelFallo(reembolso),
	}, nil
}

func (s *stripePasarela) VerificarEvento(cuerpo []byte, firma string) (Evento, error) {
	evento, err := webhook.ConstructEvent(cuerpo, firma, s.cfg.SecretoWebhook)
	if err != nil {
		return Evento{}, fmt.Errorf("%w: %w", ErrFirmaInvalida, err)
	}

	resultado := Evento{ID: evento.ID, Tipo: string(evento.Type), Crudo: cuerpo}

	// Solo se desempaqueta el objeto de los eventos que este componente
	// atiende. Los demás se registran en la bitácora igual: saber qué llegó y
	// no se procesó vale tanto como procesarlo, y sin la fila no queda rastro.
	if !strings.HasPrefix(resultado.Tipo, "payment_intent.") {
		return resultado, nil
	}

	var intencion stripe.PaymentIntent
	if err := json.Unmarshal(evento.Data.Raw, &intencion); err != nil {
		return resultado, fmt.Errorf("el evento %s trae un payment_intent ilegible: %w", evento.ID, err)
	}

	resultado.Intencion = traducirIntencion(&intencion)
	resultado.Tenant = intencion.Metadata[metadatoTenant]
	resultado.Reserva = intencion.Metadata[metadatoReserva]

	return resultado, nil
}

// Las claves de la metadata. Constantes porque las escribe quien crea la
// intención y las lee quien procesa el evento, con semanas de por medio: un
// literal repetido en dos archivos es un error de escritura que solo se nota
// cuando llega el primer webhook real.
const (
	metadatoTenant  = "tenant_id"
	metadatoReserva = "reserva_id"
)

// traducirIntencion pasa del vocabulario de Stripe al de ER-03.
//
// La tabla de estados es lo importante de esta función. Stripe tiene siete
// estados y ER-03 tres, y el reparto no es arbitrario: todo lo que todavía
// puede acabar en un cobro es `iniciado`, porque desde el punto de vista de la
// reserva la diferencia entre "falta el método de pago" y "procesando" es
// ninguna. Lo que cambia el estado de la reserva es solo `succeeded`.
func traducirIntencion(pi *stripe.PaymentIntent) Intencion {
	intencion := Intencion{
		ID:           pi.ID,
		ClientSecret: pi.ClientSecret,
		Estado:       Iniciado,
	}

	switch pi.Status {
	case stripe.PaymentIntentStatusSucceeded:
		intencion.Estado = Confirmado
	case stripe.PaymentIntentStatusCanceled:
		intencion.Estado = Fallido
	}

	// El último error se conserva aunque el estado siga siendo `iniciado`, y
	// es deliberado: una tarjeta rechazada NO cancela el PaymentIntent, lo deja
	// listo para otro intento. Contarlo como fallo definitivo cerraría el bucle
	// de reintento que RF-01 describe explícitamente.
	if pi.LastPaymentError != nil {
		intencion.MotivoFallo = pi.LastPaymentError.Msg
	}

	return intencion
}

func traducirEstadoReembolso(estado stripe.RefundStatus) EstadoReembolso {
	switch estado {
	case stripe.RefundStatusSucceeded:
		return ReembolsoConfirmado
	case stripe.RefundStatusFailed, stripe.RefundStatusCanceled:
		return ReembolsoFallido
	default:
		// pending y requires_action. Un reembolso puede tardar días en según
		// qué medio de pago, y darlo por bueno antes de tiempo es decirle a
		// alguien que ya tiene su dinero cuando no lo tiene.
		return ReembolsoPendiente
	}
}

func razonDelFallo(r *stripe.Refund) string {
	if r.FailureReason == "" {
		return ""
	}
	return string(r.FailureReason)
}

// esNoEncontrado distingue el 404 de Stripe del resto de sus errores.
//
// Importa porque los dos casos se tratan distinto: un objeto que no existe es
// una inconsistencia entre nuestra base y la suya —y hay que reconciliarla—
// mientras que un error de red es algo que se reintenta sin más.
func esNoEncontrado(err error) bool {
	var stripeErr *stripe.Error
	if errors.As(err, &stripeErr) {
		return stripeErr.Code == stripe.ErrorCodeResourceMissing
	}
	return false
}
