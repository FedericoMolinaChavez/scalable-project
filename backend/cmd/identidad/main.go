// Binario identidad — Servicio de Identidad (ARQ-01).
//
// Vive en el borde, junto al Gateway, y resuelve todo lo que tiene que ver con
// quién es alguien: el código del invitado que vuelve a ver lo suyo (RF-02), el
// alta y el perfil de una cuenta (RF-24, RF-22), el inicio de sesión con
// contraseña o magic link con su par de tokens (RF-12), la verificación de
// contacto (RF-19), la recuperación de contraseña (RF-18), las sesiones y la
// baja (RF-25), las preferencias de aviso (RF-21) y los tokens de agente
// (RF-13).
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
		Identidad: identidad.Nuevo(bd, emisor, firmante, limites, registro, identidad.Opciones{
			TTLCodigo:        cfg.Identidad.TTLCodigo,
			TTLEnlace:        cfg.Identidad.TTLEnlace,
			TTLRefresco:      cfg.Identidad.TTLRefresco,
			MaxIntentos:      cfg.Identidad.MaxIntentos,
			MaxEnviosHora:    cfg.Identidad.MaxEnviosHora,
			MaxIntentosLogin: cfg.Identidad.MaxIntentosLogin,
			BloqueoLogin:     cfg.Identidad.BloqueoLogin,
			BaseURL:          cfg.Identidad.URLApp,
		}),
		Verificador: firmante,
	}, registro, cfg.TiempoPeticion)

	registro.Info("identidad montada",
		slog.Duration("ttl_codigo", cfg.Identidad.TTLCodigo),
		slog.Duration("ttl_enlace", cfg.Identidad.TTLEnlace),
		slog.Duration("ttl_acceso", cfg.Identidad.TTLAcceso),
		slog.Duration("ttl_refresco", cfg.Identidad.TTLRefresco),
		slog.String("url_app", cfg.Identidad.URLApp),
		slog.String("smtp", cfg.Identidad.SMTPHost+":"+cfg.Identidad.SMTPPuerto))
	return nil
}
