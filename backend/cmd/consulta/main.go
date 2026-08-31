// Binario consulta — Consulta de Reservas (ARQ-01).
//
// Ruta de lectura: RF-02, RF-03 y las métricas de RF-11. Lee contra las
// réplicas de PostgreSQL, nunca contra el primario, y tolera el retraso de
// replicación que RNF-10 autoriza.
package main

import (
	"log/slog"
	"os"
)

func main() {
	registro := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	registro.Info("arranque pendiente", "servicio", "consulta", "fase", 3)
}
