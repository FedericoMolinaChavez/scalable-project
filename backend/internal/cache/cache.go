// Package cache es la única puerta a Valkey, con la misma disciplina que
// internal/datos es la única puerta a PostgreSQL.
//
// ARQ-01 le da a Valkey dos trabajos y solo dos:
//
//	Proyecciones de disponibilidad (RNF-01, RNF-03, RNF-10)
//	  El servicio de disponibilidad es el 90% del tráfico y sirve desde aquí
//	  con los 2 s de desactualización que RNF-10 autoriza.
//
//	Cuotas por cuenta y agente (RNF-08)
//	  La segunda de las tres capas de defensa: Cilium por IP, Valkey por
//	  cuenta, PostgreSQL por transacción.
//
// Y una frase que gobierna todo lo demás, literal de ARQ-01: **nunca es
// autoridad sobre el cupo; esa es siempre PostgreSQL**. Nada de lo que hay aquí
// decide si una reserva cabe. Cuando el caché y el motor discrepan, el motor
// tiene razón por definición, y ese desacuerdo es justo lo que RNF-10 autoriza.
//
// De ahí sale la regla de fallo: un Valkey caído NO puede tumbar el servicio.
// Lo que hay aquí acelera y acota; no es una dependencia dura, y por eso no
// entra en la sonda /listo.
package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go"
)

// ErrVacio es un fallo de caché: la clave no estaba. No es un error que
// registrar ni propagar, es el caso normal la primera vez.
var ErrVacio = errors.New("la clave no está en el caché")

// Cliente envuelve la conexión a Valkey. No expone el cliente crudo: si lo
// hiciera, cualquier paquete podría escribir en Valkey algo que después alguien
// tomara por autoridad, que es exactamente lo que ARQ-01 prohíbe.
type Cliente struct {
	valkey valkey.Client
}

// Abrir conecta con Valkey.
//
// La URL viene en formato redis:// porque Valkey habla el mismo protocolo y las
// herramientas del ecosistema usan ese esquema; es lo que ya está en
// .env.example.
func Abrir(url string) (*Cliente, error) {
	opciones, err := valkey.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("URL de Valkey inválida: %w", err)
	}

	// Sin caché del lado del cliente. valkey-go la ofrece y aquí estorba: los
	// valores que se guardan ya tienen una vigencia de 2 s medida en el
	// servidor, y una segunda capa de caché en cada pod añadiría su propia
	// desactualización encima de un presupuesto que RNF-10 fija exacto.
	opciones.DisableCache = true

	cliente, err := valkey.NewClient(opciones)
	if err != nil {
		return nil, fmt.Errorf("no se pudo conectar con Valkey: %w", err)
	}

	return &Cliente{valkey: cliente}, nil
}

func (c *Cliente) Cerrar() {
	c.valkey.Close()
}

// Comprobar verifica que Valkey responde.
//
// Existe para /metrics y para diagnóstico, NO para la sonda /listo. Sacar un
// pod del balanceador porque su caché no responde apagaría también la mitad que
// sí funciona: sin caché el servicio sigue sirviendo, solo que más despacio.
func (c *Cliente) Comprobar(ctx context.Context) error {
	return c.valkey.Do(ctx, c.valkey.B().Ping().Build()).Error()
}

// Leer devuelve el valor de una clave, o ErrVacio si no está.
func (c *Cliente) Leer(ctx context.Context, clave string) ([]byte, error) {
	valor, err := c.valkey.Do(ctx, c.valkey.B().Get().Key(clave).Build()).AsBytes()
	if err != nil {
		if valkey.IsValkeyNil(err) {
			return nil, ErrVacio
		}
		return nil, err
	}
	return valor, nil
}

// Guardar escribe un valor con su vigencia.
//
// La vigencia es obligatoria, no opcional. Una clave sin caducidad en un caché
// es una fuga de memoria con buena letra: nadie se acuerda de borrarla y crece
// hasta que Valkey empieza a expulsar lo que sí hacía falta.
func (c *Cliente) Guardar(ctx context.Context, clave string, valor []byte, vigencia time.Duration) error {
	if vigencia <= 0 {
		return fmt.Errorf("vigencia inválida para %q: el caché no admite claves eternas", clave)
	}

	return c.valkey.Do(ctx,
		c.valkey.B().Set().Key(clave).Value(valkey.BinaryString(valor)).Px(vigencia).Build(),
	).Error()
}
