// Binario trabajadores — el paquete "Asíncrono" de ARQ-01.
//
// Un solo proceso con varios bucles dentro, que es como lo dibuja ARQ-01: no un
// binario por trabajador. Comparten pool de conexiones, conexión a NATS y
// apagado, y ninguno de ellos justifica por sí solo un despliegue aparte.
//
// De los ocho que lista ARQ-01, aquí están tres:
//
//	Expirador de bloqueos (RF-27)
//	Transiciones automáticas (RF-28)
//	Relay del outbox hacia NATS
//
// más el motor de notificaciones (RF-10) en su versión mínima, que es el
// consumidor que da sentido al relay. Los otros cuatro —conciliador de pagos
// (RF-33), procesador de reembolsos (RF-29), rollup de métricas (RF-11) y lista
// de espera (RF-37)— dependen de tablas o de servicios externos que todavía no
// existen.
//
// No sirve tráfico de negocio: su HTTP es solo /salud, /listo y /metrics, que
// es lo que Kubernetes necesita para saber si el pod vive y si hay que
// recogerlo.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/trabajadores"
)

func main() {
	plataforma.Ejecutar("trabajadores", montar)
}

func montar(ctx context.Context, cfg plataforma.Config, registro *slog.Logger, servidor *plataforma.Servidor) error {
	bd, err := datos.Abrir(ctx, cfg.BaseDatosURL)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		bd.Cerrar()
	}()

	servidor.AnadirComprobacion(plataforma.Comprobacion{
		Nombre:    "postgresql",
		Verificar: bd.Comprobar,
	})

	flujo, err := trabajadores.AbrirJetStream(ctx, cfg.NATSURL)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		flujo.Cerrar()
	}()

	// NATS SÍ entra en /listo, al revés que el caché de disponibilidad. La
	// diferencia es qué pasa sin él: sin caché la disponibilidad responde igual,
	// solo que más despacio; sin NATS el relay no publica nada y los eventos se
	// acumulan en el outbox sin que nadie se entere. Un trabajador que no puede
	// trabajar debe decirlo.
	servidor.AnadirComprobacion(plataforma.Comprobacion{
		Nombre:    "nats",
		Verificar: flujo.Comprobar,
	})

	// El consumidor de notificaciones vive aparte de los bucles: no despierta
	// cada N segundos, se queda escuchando. JetStream le empuja los mensajes.
	notificador := trabajadores.NuevoNotificador(
		correo.NuevoSMTP(cfg.Identidad.SMTPHost, cfg.Identidad.SMTPPuerto,
			cfg.Identidad.Remitente, registro),
		registro,
	)
	go func() {
		if err := notificador.Suscribir(ctx, flujo.Conexion()); err != nil && ctx.Err() == nil {
			registro.Error("el notificador se detuvo", slog.String("error", err.Error()))
		}
	}()

	// Los bucles corren en su propia gorutina: Escuchar bloquea hasta el
	// apagado, y los trabajadores tienen que estar trabajando mientras tanto.
	go trabajadores.Correr(ctx, registro,
		trabajadores.Expirador(bd, cfg.Trabajadores.IntervaloExpirador, registro),
		trabajadores.Transiciones(bd, cfg.Trabajadores.IntervaloTransiciones,
			cfg.Trabajadores.UmbralNoShow, registro),
		trabajadores.Relay(bd, flujo, cfg.Trabajadores.IntervaloRelay, registro),
	)

	registro.Info("trabajadores montados",
		slog.Duration("expirador", cfg.Trabajadores.IntervaloExpirador),
		slog.Duration("transiciones", cfg.Trabajadores.IntervaloTransiciones),
		slog.Duration("relay", cfg.Trabajadores.IntervaloRelay),
		slog.Duration("umbral_no_show", cfg.Trabajadores.UmbralNoShow))

	return nil
}
