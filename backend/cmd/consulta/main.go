// Binario consulta — Consulta de Reservas (ARQ-01).
//
// Ruta de lectura: RF-02, RF-03 y las métricas de RF-11. Lee contra las
// réplicas de PostgreSQL, nunca contra el primario, y tolera el retraso de
// replicación que RNF-10 autoriza.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
)

func main() {
	plataforma.Ejecutar("consulta", montar)
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

	registro.Info("consulta montada")
	return nil
}
