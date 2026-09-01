package plataforma

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

// Ejecutar es el arranque común de todos los binarios: carga configuración,
// monta registro y métricas, llama a montar para que el servicio añada lo
// suyo, y escucha hasta recibir una señal de terminación.
//
// Que sea común no es solo comodidad. Garantiza que los siete componentes de
// ARQ-01 se apaguen igual, expongan las mismas sondas y etiqueten sus métricas
// del mismo modo. Un servicio que se apaga distinto que los demás rompe un
// despliegue rodante de formas que solo se ven en producción.
func Ejecutar(servicio string, montar func(context.Context, Config, *slog.Logger, *Servidor) error) {
	cfg, err := CargarConfig(servicio)
	if err != nil {
		// Todavía no hay logger configurado: si falla la config, no hay nivel
		// ni formato que respetar.
		slog.Error("configuración inválida", slog.String("error", err.Error()))
		os.Exit(1)
	}

	registro := NuevoRegistro(cfg)

	// SIGTERM es la señal que manda Kubernetes al retirar un pod. Atenderla es
	// lo que convierte un despliegue rodante en algo transparente: sin esto el
	// proceso muere de golpe y las peticiones en vuelo se pierden.
	ctx, detener := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer detener()

	metricas := NuevasMetricas(cfg)
	servidor := NuevoServidor(cfg, registro, metricas)

	if err := montar(ctx, cfg, registro, servidor); err != nil {
		registro.Error("no se pudo montar el servicio", slog.String("error", err.Error()))
		os.Exit(1)
	}

	if err := servidor.Escuchar(ctx); err != nil {
		registro.Error("el servidor terminó con error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}
