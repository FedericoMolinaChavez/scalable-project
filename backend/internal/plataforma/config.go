package plataforma

import (
	"fmt"
	"log/slog"
	"os"
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
}

// direccionesPorDefecto reparte los puertos locales.
//
// En despliegue cada componente es un pod con su propio :8080 y esto no aplica:
// existe para que dos servicios corriendo con `go run` en la misma máquina no
// se peleen por el puerto. Sin ello, arrancar el segundo falla con un
// "address already in use" que no dice cuál de los dos era.
var direccionesPorDefecto = map[string]string{
	"nucleo":   ":8080",
	"consulta": ":8081",
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

	return cfg, nil
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
