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
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
)

func main() {
	plataforma.Ejecutar("nucleo", montar)
}

func montar(ctx context.Context, cfg plataforma.Config, registro *slog.Logger, servidor *plataforma.Servidor) error {
	bd, err := datos.Abrir(ctx, cfg.BaseDatosURL)
	if err != nil {
		return err
	}

	// El pool se cierra cuando termina el contexto, no con un defer: montar
	// devuelve enseguida y el servidor sigue vivo después.
	go func() {
		<-ctx.Done()
		bd.Cerrar()
	}()

	// La base entra en /listo pero no en /salud. Si PostgreSQL cae, este pod
	// debe salir del balanceador, no reiniciarse: reiniciar los 45 pods de
	// ARQ-03 en cadena mientras la base vuelve solo empeora la recuperación.
	servidor.AnadirComprobacion(plataforma.Comprobacion{
		Nombre:    "postgresql",
		Verificar: bd.Comprobar,
	})

	registro.Info("núcleo montado")
	return nil
}
