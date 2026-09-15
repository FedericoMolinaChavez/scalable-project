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

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/almacen"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/catalogo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/consulta"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/disponibilidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pagos"
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

	// El almacén de objetos, para firmar los enlaces de los comprobantes de
	// RF-34. Firmar es un cálculo local con la clave secreta, no una llamada a
	// MinIO, así que esto no suma latencia a la respuesta ni depende de que
	// MinIO esté en pie para responder con el número del comprobante.
	//
	// Tampoco entra en /listo, y por el mismo motivo que el caché: sin él la
	// ruta sigue devolviendo el comprobante, solo que sin enlace de descarga.
	// Sacar el pod del balanceador apagaría además el catálogo, la
	// disponibilidad y el listado de reservas, que no tienen nada que ver.
	var documentos pagos.Almacen
	if objetos, err := almacen.Abrir(ctx, almacen.Config{
		Endpoint:  cfg.Almacen.Endpoint,
		AccessKey: cfg.Almacen.AccessKey,
		SecretKey: cfg.Almacen.SecretKey,
		Seguro:    cfg.Almacen.Seguro,
	}); err != nil {
		registro.Warn("sin almacén de objetos: los comprobantes irán sin enlace de descarga",
			slog.String("error", err.Error()))
	} else {
		documentos = objetos
	}

	// Solo VERIFICA tokens, nunca los emite: no construye el servicio de
	// identidad entero —con su conexión SMTP y sus límites de envío— para algo
	// que no va a hacer. El firmante basta porque con HMAC firmar y verificar
	// son la misma clave, y ese es exactamente el punto a revisar el día que
	// esto salga de un solo módulo Go: entonces toca firma asimétrica y aquí
	// solo viajaría la clave pública.
	rutas.Montar(servidor, rutas.Componentes{
		Catalogo:       catalogo.Nuevo(bd),
		Disponibilidad: disponibilidad.Nuevo(bd, proyecciones, registro),
		Consulta:       consulta.Nuevo(bd),

		// Solo la mitad lectora del componente de pagos: el comprobante ya
		// emitido (RF-34). Sin pasarela, porque leer un comprobante no llama a
		// Stripe y repartir la clave secreta a un servicio que no la usa solo
		// aumenta las formas de filtrarla.
		Comprobantes: pagos.NuevoLector(bd, documentos, registro),

		Verificador: identidad.NuevoFirmante(cfg.Identidad.TokenSecreto, cfg.Identidad.TTLAcceso),
	}, registro, cfg.TiempoPeticion)

	registro.Info("consulta montada")
	return nil
}
