// Binario identidad — Servicio de Identidad (ARQ-01).
//
// Vive en el borde, junto al Gateway, y por ahora resuelve solo RF-02: que
// quien reservó como invitado pueda demostrar que es él y volver a ver lo suyo.
// RF-12 —contraseñas, magic link, 2FA, sesiones con refresco— es el siguiente
// inquilino de este binario, no otro.
//
// Es un dominio de fallo propio y por eso es un proceso propio: manda correo,
// y un relé SMTP lento o caído no puede arrastrar consigo la ruta de reserva.
// Que aquí no se pueda pedir un código no impide que nadie reserve.
package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/rutas"
)

func main() {
	plataforma.Ejecutar("identidad", montar)
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

	// El SMTP NO entra en /listo, y la diferencia importa. La base es una
	// dependencia dura: sin ella este servicio no puede hacer nada. El correo
	// no lo es del todo: con el relé caído se sigue pudiendo canjear un código
	// ya enviado, y sacar el pod del balanceador por eso apagaría también esa
	// mitad que sí funciona.
	emisor := correo.NuevoSMTP(
		cfg.Identidad.SMTPHost, cfg.Identidad.SMTPPuerto, cfg.Identidad.Remitente, registro)

	firmante := identidad.NuevoFirmante(cfg.Identidad.TokenSecreto, cfg.Identidad.TTLAcceso)

	// Valkey: la segunda capa de RNF-08. Este servicio SÍ la necesita para
	// funcionar, al revés que el caché de disponibilidad: el límite de envíos
	// falla cerrado porque detrás no hay ninguna otra capa que impida mandar
	// correo. Sin Valkey no se puede pedir un código —pero sí canjear uno ya
	// enviado, que es la mitad que no depende de esto—, y por eso tampoco
	// entra en /listo.
	limites, err := cache.Abrir(cfg.ValkeyURL)
	if err != nil {
		return fmt.Errorf("no se pudo abrir Valkey, que impone las cuotas de RNF-08: %w", err)
	}
	go func() {
		<-ctx.Done()
		limites.Cerrar()
	}()

	rutas.Montar(servidor, rutas.Componentes{
		Identidad: identidad.Nuevo(
			bd, emisor, firmante, limites, registro,
			cfg.Identidad.TTLCodigo, cfg.Identidad.MaxIntentos, cfg.Identidad.MaxEnviosHora),
		Verificador: firmante,
	}, registro, cfg.TiempoPeticion)

	registro.Info("identidad montada",
		slog.Duration("ttl_codigo", cfg.Identidad.TTLCodigo),
		slog.Duration("ttl_acceso", cfg.Identidad.TTLAcceso),
		slog.String("smtp", cfg.Identidad.SMTPHost+":"+cfg.Identidad.SMTPPuerto))
	return nil
}
