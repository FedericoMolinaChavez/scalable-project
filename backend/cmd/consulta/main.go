// Binario consulta — Consulta de Reservas (ARQ-01).
//
// Ruta de lectura: RF-02, RF-03 y las métricas de RF-11. Lee contra las
// réplicas de PostgreSQL, nunca contra el primario, y tolera el retraso de
// replicación que RNF-10 autoriza.
//
// Monta además la disponibilidad, que en ARQ-01 es un componente propio y
// comparte con este la propiedad que decide dónde vive: `disp --> pgr`, contra
// las réplicas, igual que `consulta`. Separarlos será mover una línea.
//
// El catálogo YA no está aquí. En ARQ-01 "Configuración y Catálogo" apunta a
// `pgb` —al primario— porque escribe, y esa flecha es la que lo saca de este
// binario a `cmd/configuracion`. Lo que NO se mezcla, y sigue sin mezclarse, es
// la escritura de reservas: esa está en `nucleo` y ahí se queda.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
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

	// El caché de las proyecciones de disponibilidad (RNF-01, RNF-03, RNF-10).
	// Es el 90% del tráfico y por eso existe: servirlo desde aquí es lo que
	// hace sostenible ese número contra las réplicas.
	//
	// Y NO entra en /listo. Si Valkey cae, la disponibilidad sigue
	// respondiendo desde PostgreSQL —más despacio, pero respondiendo—, así que
	// sacar el pod del balanceador apagaría también la mitad que funciona. Es
	// el mismo criterio que se aplicó al SMTP en cmd/identidad.
	var proyecciones disponibilidad.Caché
	if valkey, err := cache.Abrir(cfg.ValkeyURL); err != nil {
		registro.Warn("sin caché de disponibilidad: cada consulta irá a PostgreSQL",
			slog.String("error", err.Error()))
	} else {
		proyecciones = valkey
		go func() {
			<-ctx.Done()
			valkey.Cerrar()
		}()
	}

	// Solo VERIFICA tokens, nunca los emite: no construye el servicio de
	// identidad entero —con su conexión SMTP y sus límites de envío— para algo
	// que no va a hacer. El firmante basta porque con HMAC firmar y verificar
	// son la misma clave, y ese es exactamente el punto a revisar el día que
	// esto salga de un solo módulo Go: entonces toca firma asimétrica y aquí
	// solo viajaría la clave pública.
	rutas.Montar(servidor, rutas.Componentes{
		Disponibilidad: disponibilidad.Nuevo(bd, proyecciones, registro),
		Consulta:       consulta.Nuevo(bd),
		Verificador:    identidad.NuevoFirmante(cfg.Identidad.TokenSecreto, cfg.Identidad.TTLAcceso),
	}, registro, cfg.TiempoPeticion)

	registro.Info("consulta montada")
	return nil
}
