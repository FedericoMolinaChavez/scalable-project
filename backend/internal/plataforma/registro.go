package plataforma

import (
	"log/slog"
	"os"
)

// NuevoRegistro construye el logger estructurado del servicio.
//
// JSON siempre que no sea desarrollo, porque el destino es Loki (ARQ-01) y un
// registro en texto plano obliga a parsearlo con expresiones regulares que se
// rompen en cuanto un mensaje lleva un salto de línea.
//
// Cada línea lleva el nombre del servicio: con 45 pods de siete binarios
// distintos escribiendo al mismo sitio (ARQ-03), un registro sin origen no
// sirve para nada.
func NuevoRegistro(cfg Config) *slog.Logger {
	opciones := &slog.HandlerOptions{Level: cfg.NivelRegistro}

	var manejador slog.Handler
	if cfg.EnDesarrollo() {
		manejador = slog.NewTextHandler(os.Stdout, opciones)
	} else {
		manejador = slog.NewJSONHandler(os.Stdout, opciones)
	}

	return slog.New(manejador).With(
		slog.String("servicio", cfg.Servicio),
		slog.String("entorno", cfg.Entorno),
	)
}
