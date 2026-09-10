// Binario trabajadores — el paquete "Asíncrono" de ARQ-01.
//
// Un solo proceso con varios bucles dentro, que es como lo dibuja ARQ-01: no un
// binario por trabajador. Comparten pool de conexiones, conexión a NATS y
// apagado, y ninguno de ellos justifica por sí solo un despliegue aparte.
//
// Están los ocho que lista ARQ-01:
//
//	Expirador de bloqueos (RF-27)
//	Transiciones automáticas (RF-28)
//	Relay del outbox hacia NATS
//	Conciliador de pagos (RF-33)
//	Procesador de reembolsos (RF-29)
//	Emisor de comprobantes (RF-34)
//	Rollup de métricas (RF-11)
//	Lista de espera (RF-37)
//
// más el motor de notificaciones (RF-10) en su versión mínima, que es el
// consumidor que da sentido al relay.
//
// Los tres últimos de la lista de arriba tienen ritmos muy distintos —diez
// segundos los comprobantes, cinco minutos las métricas— y aun así comparten
// proceso. Es lo que dibuja ARQ-01 y sigue siendo correcto: comparten pool de
// conexiones, apagado y contexto de tenant, y ninguno consume lo suficiente
// como para justificar un despliegue propio. El día que uno lo justifique, sale
// de aquí sin tocar a los demás: cada uno es un Bucle independiente.
//
// El de la lista de espera corre y no encuentra nada hasta que exista RF-12:
// negocio.lista_espera.cuenta_id es NOT NULL y no hay cuentas. Está montado
// igualmente porque su lógica no depende de eso, y porque un trabajador que
// existe y no encuentra trabajo es más honesto que uno que falta.
//
// No sirve tráfico de negocio: su HTTP es solo /salud, /listo y /metrics, que
// es lo que Kubernetes necesita para saber si el pod vive y si hay que
// recogerlo.
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/almacen"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/trabajadores"
)

func main() {
	plataforma.Ejecutar("trabajadores", montar)
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

	flujo, err := trabajadores.AbrirJetStream(ctx, cfg.NATSURL)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		flujo.Cerrar()
	}()

	// NATS SÍ entra en /listo, al revés que el caché de disponibilidad. La
	// diferencia es qué pasa sin él: sin caché la disponibilidad responde igual,
	// solo que más despacio; sin NATS el relay no publica nada y los eventos se
	// acumulan en el outbox sin que nadie se entere. Un trabajador que no puede
	// trabajar debe decirlo.
	servidor.AnadirComprobacion(plataforma.Comprobacion{
		Nombre:    "nats",
		Verificar: flujo.Comprobar,
	})

	// El consumidor de notificaciones vive aparte de los bucles: no despierta
	// cada N segundos, se queda escuchando. JetStream le empuja los mensajes.
	notificador := trabajadores.NuevoNotificador(
		correo.NuevoSMTP(cfg.Identidad.SMTPHost, cfg.Identidad.SMTPPuerto,
			cfg.Identidad.Remitente, registro),
		registro,
	)
	go func() {
		if err := notificador.Suscribir(ctx, flujo.Conexion()); err != nil && ctx.Err() == nil {
			registro.Error("el notificador se detuvo", slog.String("error", err.Error()))
		}
	}()

	// La pasarela de pago. La necesitan dos bucles: el conciliador, para
	// preguntar por los cobros cuyo webhook no llegó (RF-33), y el de
	// reembolsos, para devolver dinero (RF-29). Es la misma implementación que
	// usa el binario `pagos`; lo que cambia es la dirección en que se habla con
	// Stripe —aquí siempre saliente, allí también entrante—.
	pasarela := pagos.NuevaStripe(pagos.StripeConfig{
		ClaveSecreta:    cfg.Pagos.ClaveSecreta,
		ClavePublicable: cfg.Pagos.ClavePublicable,
		SecretoWebhook:  cfg.Pagos.SecretoWebhook,
	})

	// El almacén de objetos, para los comprobantes de RF-34. Es el único
	// inquilino de MinIO en todo ARQ-01.
	//
	// No entra en /listo y su ausencia no impide arrancar: sin él los
	// comprobantes se emiten igual —con su número y sus datos congelados— y lo
	// único que falta es el documento, que se sube en cuanto MinIO vuelva. Es
	// el mismo criterio que el caché en `consulta`: degradar es mejor que
	// apagar, siempre que la degradación sea visible y se cure sola.
	var documentos trabajadores.Almacen
	if objetos, err := almacen.Abrir(ctx, almacen.Config{
		Endpoint:  cfg.Almacen.Endpoint,
		AccessKey: cfg.Almacen.AccessKey,
		SecretKey: cfg.Almacen.SecretKey,
		Seguro:    cfg.Almacen.Seguro,
	}); err != nil {
		registro.Warn("sin almacén de objetos: los comprobantes se emitirán sin documento",
			slog.String("error", err.Error()))
	} else {
		documentos = objetos
	}

	// Los bucles corren en su propia gorutina: Escuchar bloquea hasta el
	// apagado, y los trabajadores tienen que estar trabajando mientras tanto.
	//
	// Los ocho van en la MISMA llamada a Correr y no en ocho gorutinas sueltas:
	// Correr los arranca a todos y espera a que todos terminen, así que el
	// apagado ordenado del proceso no puede cortar a uno a mitad de su pasada
	// mientras espera a otro.
	go trabajadores.Correr(ctx, registro,
		trabajadores.Expirador(bd, cfg.Trabajadores.IntervaloExpirador, registro),
		trabajadores.Transiciones(bd, cfg.Trabajadores.IntervaloTransiciones,
			cfg.Trabajadores.UmbralNoShow, registro),
		trabajadores.Relay(bd, flujo, cfg.Trabajadores.IntervaloRelay, registro),

		// El orden en esta lista no importa —corren en paralelo— pero se
		// agrupan por tema para que se lea qué hace este proceso: primero lo
		// que mueve reservas, luego lo que mueve dinero, luego lo que sirve
		// para mirar.
		trabajadores.Conciliador(bd, pasarela, cfg.Trabajadores.IntervaloConciliador,
			cfg.Trabajadores.GraciaConciliacion, registro),
		trabajadores.Reembolsos(bd, pasarela, cfg.Trabajadores.IntervaloReembolsos, registro),
		trabajadores.Comprobantes(bd, documentos, cfg.Trabajadores.IntervaloComprobantes, registro),

		trabajadores.Metricas(bd, cfg.Trabajadores.IntervaloMetricas, registro),
		trabajadores.Espera(bd, cfg.Trabajadores.IntervaloEspera, registro),
	)

	registro.Info("trabajadores montados",
		slog.Duration("expirador", cfg.Trabajadores.IntervaloExpirador),
		slog.Duration("transiciones", cfg.Trabajadores.IntervaloTransiciones),
		slog.Duration("relay", cfg.Trabajadores.IntervaloRelay),
		slog.Duration("conciliador", cfg.Trabajadores.IntervaloConciliador),
		slog.Duration("reembolsos", cfg.Trabajadores.IntervaloReembolsos),
		slog.Duration("comprobantes", cfg.Trabajadores.IntervaloComprobantes),
		slog.Duration("metricas", cfg.Trabajadores.IntervaloMetricas),
		slog.Duration("espera", cfg.Trabajadores.IntervaloEspera),
		slog.Duration("umbral_no_show", cfg.Trabajadores.UmbralNoShow),
		slog.Duration("gracia_conciliacion", cfg.Trabajadores.GraciaConciliacion))

	return nil
}
