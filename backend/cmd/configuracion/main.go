// Binario configuracion — Configuración y Catálogo (ARQ-01).
//
// Es el componente que en ARQ-01 aparece dentro del "Núcleo síncrono" y que va
// contra el PRIMARIO (`config --> pgb`), no contra las réplicas. Esa flecha es
// la razón de que sea un binario propio y no siga colgando de `consulta`:
// aquella lee de las réplicas y tolera el retraso de replicación que RNF-10
// autoriza, y esto ESCRIBE. Un administrador que publica una regla de
// disponibilidad contra una réplica no publica nada.
//
// Sirve las dos caras del mismo componente:
//
//	pública        el catálogo que mira el cliente para reservar (RF-26)
//	administrador  lo que hace posible reservar: sedes, servicios y recursos
//	               (RF-30), disponibilidad y excepciones (RF-14), políticas
//	               (RF-15), vouchers (RF-17) y tarifas (RF-31)
//
// Y la lectura de la auditoría (RF-36), que vive aquí y no en `consulta` por lo
// mismo: la traza de una acción crítica se escribe en la misma transacción que
// la acción, así que quien la consulta debe poder ver lo que acaba de ocurrir
// sin esperar a que replique.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/auditoria"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/rutas"
)

func main() {
	plataforma.Ejecutar("configuracion", montar)
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

	// Solo VERIFICA tokens, nunca los emite, igual que `consulta` y `nucleo`:
	// no construye el servicio de identidad entero —con su conexión SMTP y sus
	// límites de envío— para algo que no va a hacer.
	rutas.Montar(servidor, rutas.Componentes{
		Catalogo:    catalogo.Nuevo(bd),
		Auditoria:   auditoria.NuevaConsulta(bd),
		Verificador: identidad.NuevoFirmante(cfg.Identidad.TokenSecreto, cfg.Identidad.TTLAcceso),
	}, registro, cfg.TiempoPeticion)

	registro.Info("configuración montada")
	return nil
}
