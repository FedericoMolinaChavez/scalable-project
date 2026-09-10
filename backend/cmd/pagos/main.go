// Binario pagos — el componente "Webhook de Pagos" de ARQ-01.
//
// Es un proceso propio por la nota que ARQ-01 le pone al lado: su
// disponibilidad la acota Stripe, así que vive fuera del presupuesto de RNF-04,
// y por eso la reserva se confirma aquí y no en la ruta síncrona. Es la misma
// regla que separa al resto —frontera transaccional y dominio de fallo— y el
// pago la cumple por partida doble: depende de un tercero y tolera latencia.
//
// La consecuencia práctica de esa separación es la que importa: si Stripe se
// cae, este proceso deja de abrir cobros nuevos y NADIE deja de reservar. El
// núcleo sigue apartando cupos con su restricción EXCLUDE, la disponibilidad
// sigue respondiendo, y las reservas pendientes vencen solas como siempre.
//
// Sirve tres rutas del contrato y una que no está en él:
//
//	POST /v1/pagos/intencion            abre el cobro
//	GET  /v1/pagos/{id}/estado          dónde está la confirmación asíncrona
//	POST /v1/webhooks/stripe            la trae; no es del contrato porque no
//	                                    la llama ningún cliente de esta API
package main

import (
	"context"
	"log/slog"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/plataforma"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/rutas"
)

func main() {
	plataforma.Ejecutar("pagos", montar)
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

	// PostgreSQL entra en /listo y Stripe NO, y la asimetría es la misma que en
	// los otros binarios. Sin base, este proceso no puede hacer nada: ni
	// registrar un webhook ni leer una reserva. Sin Stripe puede seguir
	// aplicando los eventos de cobros ya hechos, que es la mitad que de verdad
	// no puede esperar —un cobro sin aplicar es dinero cobrado y cupo sin
	// confirmar— y sacar el pod del balanceador la apagaría también.
	servidor.AnadirComprobacion(plataforma.Comprobacion{
		Nombre:    "postgresql",
		Verificar: bd.Comprobar,
	})

	rutas.Montar(servidor, rutas.Componentes{
		Pagos: pagos.Nuevo(bd, pagos.NuevaStripe(pagos.StripeConfig{
			ClaveSecreta:    cfg.Pagos.ClaveSecreta,
			ClavePublicable: cfg.Pagos.ClavePublicable,
			SecretoWebhook:  cfg.Pagos.SecretoWebhook,
		}), nil, registro),
	}, registro, cfg.TiempoPeticion)

	// No lleva Verificador y no le hace falta: las tres rutas que monta son
	// públicas. Las dos del cliente porque RF-01 admite reservar como invitado
	// —quien no tiene cuenta también tiene que poder pagar— y el webhook porque
	// se autentica con la firma de Stripe, que es más fuerte que un token
	// nuestro y no la emite este sistema.

	// El almacén de comprobantes tampoco: emitirlos es trabajo del binario de
	// trabajadores y leerlos, del de consulta. Este proceso ni escribe ni lee
	// documentos.

	registro.Info("pagos montado", slog.String("webhook", rutas.RutaWebhookStripe))
	return nil
}
