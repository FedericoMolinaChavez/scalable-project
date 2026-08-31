// Binario nucleo — Núcleo de Reservas (ARQ-01).
//
// Único componente que escribe en negocio.reserva por la ruta síncrona, y por
// tanto dueño de la invariante de RNF-10: en una sola transacción valida la
// cuota de la cuenta, inserta la reserva pendiente contra la restricción
// EXCLUDE, consume el voucher y escribe el outbox.
//
// Presupuesto de latencia: 200 ms (RNF-01).
package main

import (
	"log/slog"
	"os"
)

func main() {
	registro := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	registro.Info("arranque pendiente", "servicio", "nucleo", "fase", 3)
}
