// Binario consulta — Consulta de Reservas (ARQ-01).
//
// Ruta de lectura: RF-02, RF-03 y las métricas de RF-11. Lee contra las
// réplicas de PostgreSQL, nunca contra el primario, y tolera el retraso de
// replicación que RNF-10 autoriza.
//
// Monta además el catálogo (RF-26) y la disponibilidad, que en ARQ-01 son dos
// componentes propios —"Configuración y Catálogo" y "Servicio de
// Disponibilidad"— y acabarán siendo sus propios binarios cuando tengan
// despliegue propio. Su código ya vive separado en internal/catalogo e
// internal/disponibilidad, así que separarlos será mover estas dos líneas. Lo
// que NO se mezcla es la escritura: esa está en `nucleo` y ahí se queda, porque
// es la frontera que la descomposición de ARQ-01 dice que importa.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/rutas"
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

	rutas.Montar(servidor, rutas.Componentes{
		Catalogo:       catalogo.Nuevo(bd),
		Disponibilidad: disponibilidad.Nuevo(bd),
		Consulta:       consulta.Nuevo(bd),
	}, registro, cfg.TiempoPeticion)

	registro.Info("consulta montada")
	return nil
}
