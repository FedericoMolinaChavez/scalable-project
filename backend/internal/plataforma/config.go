package plataforma

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"
)

// Config es lo que todo servicio necesita para arrancar. Se lee del entorno y
// no de un archivo: es lo que espera un despliegue en Kubernetes, donde los
// valores llegan de ConfigMaps y Secrets (ARQ-03).
type Config struct {
	// Servicio identifica al binario en registros, métricas y trazas. Sin él,
	// los registros de los 45 pods de ARQ-03 son indistinguibles.
	Servicio string

	Entorno   string
	Direccion string

	BaseDatosURL string

	// ValkeyURL es el caché de las proyecciones de disponibilidad y el almacén
	// de las cuotas de RNF-08.
	//
	// A diferencia de DATABASE_URL, su ausencia NO impide arrancar: Valkey
	// acelera y acota, no decide. Un servicio sin caché sirve igual, solo que
	// cada consulta baja hasta las réplicas. Exigirlo convertiría una
	// optimización en una dependencia dura, que es justo lo contrario de lo
	// que dice la nota de ARQ-01.
	ValkeyURL string

	NivelRegistro slog.Level

	// TiempoApagado acota cuánto se espera a que terminen las peticiones en
	// curso antes de cerrar a la fuerza. Debe ser mayor que el presupuesto de
	// RNF-01 y menor que el terminationGracePeriodSeconds del pod, o
	// Kubernetes mata el proceso a mitad de una transacción.
	TiempoApagado time.Duration

	// TiempoPeticion acota una petición individual cancelando su contexto, que
	// es lo único que aborta de verdad la consulta y devuelve la conexión al
	// pooler. Los plazos del http.Server cortan el socket pero dejan al
	// manejador trabajando para nadie, reteniendo una conexión de PgBouncer.
	//
	// Es una red de seguridad, no el presupuesto de RNF-01: llegar aquí ya
	// significa haberlo incumplido por mucho.
	TiempoPeticion time.Duration

	// TTLReserva es cuánto vive un bloqueo antes de vencer (RF-27). Solo lo usa
	// el núcleo; está en la configuración común porque RF-27 pide "un TTL
	// definido" sin fijarlo, y un número así pertenece al despliegue.
	TTLReserva time.Duration

	// NATSURL es el bus donde el relay publica el outbox.
	//
	// A diferencia de Valkey, sin esto un trabajador no puede trabajar: los
	// eventos se quedarían acumulándose en el outbox sin que nadie los
	// entregue. Por eso el binario de trabajadores se niega a arrancar sin
	// NATS, y su sonda /listo lo incluye.
	NATSURL string

	// Identidad agrupa lo que necesita el servicio de identidad (RF-02).
	Identidad Identidad

	// Trabajadores agrupa los ritmos de los bucles asíncronos.
	Trabajadores Trabajadores

	// Pagos son las credenciales del proveedor (RF-33).
	Pagos Pagos

	// Almacen es el almacenamiento de objetos donde viven los comprobantes de
	// RF-34.
	Almacen Almacen
}

// Pagos son las tres credenciales de Stripe.
//
// Las tres son de despliegue y ninguna tiene valor por defecto razonable: una
// clave de prueba embebida en el código acabaría en producción el día que
// alguien olvide el Secret, y entonces los cobros se harían contra una cuenta
// que no es la del negocio.
type Pagos struct {
	// ClaveSecreta autoriza a crear cobros. Es la credencial más peligrosa del
	// sistema y solo la lleva el binario `pagos`.
	ClaveSecreta string

	// ClavePublicable inicializa Stripe.js en el navegador. Se sirve desde la
	// API (ver el contrato) porque es del entorno y no del artefacto del
	// frontend.
	ClavePublicable string

	// SecretoWebhook firma los eventos entrantes. Sin él, cualquiera que
	// conozca la URL puede confirmar reservas que nadie pagó, así que el
	// binario se niega a arrancar si falta: es la misma decisión que con
	// TOKEN_SECRETO, y por la misma razón.
	SecretoWebhook string
}

// Almacen es cómo se llega a MinIO.
type Almacen struct {
	Endpoint  string
	AccessKey string
	SecretKey string

	// Seguro distingue http de https. En desarrollo MinIO va sin TLS; en
	// despliegue no.
	Seguro bool
}

// Trabajadores es cada cuánto despierta cada bucle.
//
// Son configuración y no constantes porque son compromisos, no verdades. El del
// expirador es el más caro de los tres: determina cuánto tiempo un cupo libre
// sigue pareciendo ocupado en la agenda de todo el mundo. Bajarlo mejora esa
// latencia y sube la carga de barrido; subirlo hace lo contrario.
type Trabajadores struct {
	IntervaloExpirador    time.Duration
	IntervaloTransiciones time.Duration
	IntervaloRelay        time.Duration

	// IntervaloConciliador es cada cuánto se le pregunta a Stripe por los pagos
	// cuyo webhook no llegó (RF-33). Es el más lento de todos a propósito: cada
	// pasada son llamadas a un tercero con cuota, y el camino normal es que el
	// webhook llegue en segundos.
	IntervaloConciliador time.Duration

	// IntervaloReembolsos es cada cuánto se drenan los reembolsos pendientes
	// (RF-29).
	IntervaloReembolsos time.Duration

	// IntervaloComprobantes es cada cuánto se emiten los comprobantes de los
	// pagos ya confirmados (RF-34).
	IntervaloComprobantes time.Duration

	// IntervaloMetricas es cada cuánto se recalcula el rollup de RF-11.
	IntervaloMetricas time.Duration

	// IntervaloEspera es cada cuánto se revisa si un cupo liberado tiene a
	// alguien esperándolo (RF-37).
	IntervaloEspera time.Duration

	// GraciaConciliacion es cuánto se espera a que llegue el webhook antes de
	// ir a preguntar. Corto de más multiplica llamadas a Stripe por nada; largo
	// de más deja al cliente mirando una reserva pendiente que ya pagó.
	GraciaConciliacion time.Duration

	// UmbralNoShow es cuánto se espera desde el inicio de la cita antes de
	// darla por ausencia (RF-28). Es el único de estos números que tiene
	// consecuencias sobre el dinero —un no-show dispara la política de RF-15 y
	// el reembolso de RF-29— así que es del negocio, no del despliegue, y
	// acabará viviendo por tenant cuando exista RF-16.
	UmbralNoShow time.Duration
}

// Identidad es la configuración de la puerta por la que un invitado vuelve a
// sus reservas.
type Identidad struct {
	// TokenSecreto firma los tokens de acceso. Sin él, cualquiera puede
	// fabricarse uno y leer las reservas de quien quiera, así que el servicio
	// se niega a arrancar si falta: fallar al arrancar es ruidoso y ocurre una
	// vez; arrancar sin firma es silencioso y ocurre en cada petición.
	TokenSecreto []byte

	// TTLCodigo es la vigencia del código OTP. RF-02 no la fija; se adopta la
	// de RF-19 y RF-12 para los códigos de seis dígitos.
	TTLCodigo time.Duration

	// TTLAcceso es cuánto vale el token que se entrega tras acertar el código.
	// Corto a propósito: no hay refresco ni forma de revocarlo, porque un
	// invitado no tiene sesión que gestionar (RF-25 es de cuentas). La única
	// defensa de un token que no se puede revocar es que caduque pronto.
	TTLAcceso time.Duration

	// MaxIntentos son los canjes fallidos que aguanta un código antes de
	// quemarse (RF-12 A2). Un código de seis dígitos son un millón de
	// combinaciones, que no es un número grande para una máquina.
	MaxIntentos int

	// MaxEnviosHora acota cuántos códigos se pueden pedir para un mismo
	// destino en una hora (RF-12 A11). Sin esto, el formulario de "enviarme un
	// código" es un cañón de correo apuntando a la dirección que alguien
	// escriba.
	MaxEnviosHora int

	// Remitente es el `From` de los correos.
	Remitente string

	// SMTP es donde se entregan. En desarrollo apunta a Mailpit, que captura
	// todo y no entrega nada fuera.
	SMTPHost   string
	SMTPPuerto string
}

// direccionesPorDefecto reparte los puertos locales.
//
// En despliegue cada componente es un pod con su propio :8080 y esto no aplica:
// existe para que dos servicios corriendo con `go run` en la misma máquina no
// se peleen por el puerto. Sin ello, arrancar el segundo falla con un
// "address already in use" que no dice cuál de los dos era.
var direccionesPorDefecto = map[string]string{
	"nucleo":       ":8080",
	"consulta":     ":8081",
	"identidad":    ":8082",
	"trabajadores": ":8083",
	"pagos":        ":8084",
}

// CargarConfig lee la configuración del entorno. Falla si falta algo sin
// valor por defecto razonable, en vez de arrancar a medias: un servicio que
// levanta sin base de datos solo traslada el fallo a la primera petición.
func CargarConfig(servicio string) (Config, error) {
	direccion, conocido := direccionesPorDefecto[servicio]
	if !conocido {
		direccion = ":8080"
	}

	cfg := Config{
		Servicio:       servicio,
		Entorno:        texto("ENTORNO", "desarrollo"),
		Direccion:      texto("DIRECCION", direccion),
		BaseDatosURL:   texto("DATABASE_URL", ""),
		ValkeyURL:      texto("VALKEY_URL", "redis://localhost:6379"),
		NATSURL:        texto("NATS_URL", "nats://localhost:4222"),
		TiempoApagado:  duracion("TIEMPO_APAGADO", 15*time.Second),
		TiempoPeticion: duracion("TIEMPO_PETICION", 10*time.Second),
		TTLReserva:     duracion("TTL_RESERVA", 15*time.Minute),
	}

	nivel, err := nivelRegistro(texto("NIVEL_REGISTRO", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.NivelRegistro = nivel

	if cfg.BaseDatosURL == "" {
		return Config{}, fmt.Errorf("falta DATABASE_URL")
	}

	cfg.Identidad = Identidad{
		TokenSecreto:  []byte(texto("TOKEN_SECRETO", "")),
		TTLCodigo:     duracion("TTL_CODIGO", 5*time.Minute),
		TTLAcceso:     duracion("TTL_ACCESO", 15*time.Minute),
		MaxIntentos:   entero("MAX_INTENTOS_CODIGO", 3),
		MaxEnviosHora: entero("MAX_ENVIOS_HORA", 3),
		Remitente:     texto("CORREO_REMITENTE", "reservas@localhost"),
		SMTPHost:      texto("SMTP_HOST", "localhost"),
		SMTPPuerto:    texto("SMTP_PORT", "1025"),
	}

	cfg.Trabajadores = Trabajadores{
		// Diez segundos: es la latencia con la que un cupo abandonado vuelve a
		// verse libre. Más corto no aporta —nadie mira la agenda con esa
		// frecuencia— y más largo se empieza a notar como huecos fantasma.
		IntervaloExpirador: duracion("INTERVALO_EXPIRADOR", 10*time.Second),

		// Un minuto basta: las transiciones que aplica dependen de la hora de
		// la cita, y nadie nota que su reserva pase a "en curso" con medio
		// minuto de retraso.
		IntervaloTransiciones: duracion("INTERVALO_TRANSICIONES", time.Minute),

		// Un segundo. El relay es lo que separa "la reserva existe" de "le
		// llegó el correo", así que es el único de los tres donde la latencia
		// la percibe una persona.
		IntervaloRelay: duracion("INTERVALO_RELAY", time.Second),

		UmbralNoShow: duracion("UMBRAL_NO_SHOW", 15*time.Minute),

		// Treinta segundos. El webhook de Stripe suele llegar en menos de dos,
		// así que este bucle casi nunca encuentra nada; existe para el caso en
		// que la entrega falle, y ahí lo que importa es que actúe antes de que
		// el bloqueo venza, no que actúe rápido.
		IntervaloConciliador: duracion("INTERVALO_CONCILIADOR", 30*time.Second),

		// Un minuto. Devolver dinero no es urgente al segundo, pero sí visible:
		// quien cancela quiere ver el movimiento el mismo día.
		IntervaloReembolsos: duracion("INTERVALO_REEMBOLSOS", time.Minute),

		// Diez segundos. El comprobante es lo que la persona busca justo
		// después de pagar, así que aquí la latencia sí se percibe.
		IntervaloComprobantes: duracion("INTERVALO_COMPROBANTES", 10*time.Second),

		// Cinco minutos. El panel de RF-11 mira días, no minutos: recalcular
		// más a menudo consume lecturas para mover un número que nadie está
		// mirando.
		IntervaloMetricas: duracion("INTERVALO_METRICAS", 5*time.Minute),

		// Quince segundos. Un cupo que se libera y tiene lista de espera es
		// justo el caso en que la velocidad se convierte en una reserva más.
		IntervaloEspera: duracion("INTERVALO_ESPERA", 15*time.Second),

		// Dos minutos de gracia antes de preguntarle a Stripe. Es holgadamente
		// más que lo que tarda un webhook y holgadamente menos que el TTL de un
		// bloqueo, que es la ventana donde esto tiene que actuar.
		GraciaConciliacion: duracion("GRACIA_CONCILIACION", 2*time.Minute),
	}

	cfg.Pagos = Pagos{
		ClaveSecreta:    texto("STRIPE_SECRET_KEY", ""),
		ClavePublicable: texto("STRIPE_PUBLISHABLE_KEY", ""),
		SecretoWebhook:  texto("STRIPE_WEBHOOK_SECRET", ""),
	}

	cfg.Almacen = Almacen{
		Endpoint:  texto("MINIO_ENDPOINT", "localhost:9000"),
		AccessKey: texto("MINIO_ACCESS_KEY", ""),
		SecretKey: texto("MINIO_SECRET_KEY", ""),
		Seguro:    texto("MINIO_SEGURO", "") == "true",
	}

	// Las credenciales de Stripe se exigen solo a quien cobra. El binario
	// `pagos` no puede hacer nada sin ellas: sin la clave secreta no abre
	// cobros, y sin el secreto del webhook no puede distinguir un evento de
	// Stripe de uno que se inventó cualquiera, que es peor que no recibir
	// ninguno. Fallar al arrancar es ruidoso y ocurre una vez; arrancar sin
	// verificar firmas es silencioso y confirma reservas que nadie pagó.
	if usaStripe[servicio] {
		if cfg.Pagos.ClaveSecreta == "" {
			return Config{}, fmt.Errorf(
				"falta STRIPE_SECRET_KEY: sin ella no se puede abrir ningún cobro")
		}
		if cfg.Pagos.SecretoWebhook == "" {
			return Config{}, fmt.Errorf(
				"falta STRIPE_WEBHOOK_SECRET: sin firma, cualquiera puede confirmar una reserva que nadie pagó")
		}
	}

	// El secreto lo exigen los servicios que tocan tokens: identidad los firma,
	// y los demás los verifican para saber de quién es la petición. No se lo
	// exige a un binario que no los use, porque repartir una credencial a
	// componentes que no la necesitan solo aumenta las formas de filtrarla.
	//
	// Es HMAC, así que firmar y verificar usan la MISMA clave: cualquiera que
	// pueda verificar puede también fabricar. Cuando eso deje de ser aceptable
	// —al salir de un solo módulo Go, o al validarlos en el Gateway— toca
	// firma asimétrica, y entonces solo identidad guarda la clave privada.
	if usaTokens[servicio] && len(cfg.Identidad.TokenSecreto) == 0 {
		return Config{}, fmt.Errorf(
			"falta TOKEN_SECRETO: sin firma, cualquiera puede fabricarse un token de acceso")
	}

	return cfg, nil
}

var usaTokens = map[string]bool{
	"identidad": true, // los firma
	"consulta":  true, // los verifica para acotar RF-02
	"nucleo":    true, // los verifica para cancelar (RF-06)
}

// usaStripe son los binarios que hablan con el proveedor de pago.
//
// `consulta` NO está, aunque sirva el comprobante de RF-34: leer un comprobante
// ya emitido no llama a Stripe. Repartir la clave secreta a un componente que
// no la usa solo aumenta las formas de filtrarla.
var usaStripe = map[string]bool{
	"pagos":        true, // abre los cobros y recibe los webhooks
	"trabajadores": true, // concilia (RF-33) y reembolsa (RF-29)
}

// EnDesarrollo distingue el entorno local del desplegado. Se usa para decidir
// el formato de los registros, nunca para cambiar el comportamiento del
// dominio: una regla de negocio que solo se cumple en producción no está
// probada en ningún sitio.
func (c Config) EnDesarrollo() bool {
	return c.Entorno == "desarrollo"
}

func texto(clave, porDefecto string) string {
	if v, existe := os.LookupEnv(clave); existe && v != "" {
		return v
	}
	return porDefecto
}

func duracion(clave string, porDefecto time.Duration) time.Duration {
	v, existe := os.LookupEnv(clave)
	if !existe || v == "" {
		return porDefecto
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return porDefecto
	}
	return d
}

func entero(clave string, porDefecto int) int {
	v, existe := os.LookupEnv(clave)
	if !existe || v == "" {
		return porDefecto
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return porDefecto
	}
	return n
}

func nivelRegistro(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("NIVEL_REGISTRO no reconocido: %q", s)
	}
}
