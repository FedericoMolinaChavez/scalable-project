package cache

import (
	"context"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Límites de tasa: la segunda de las tres capas de defensa de RNF-08.
//
// Las tres, según la nota del Gateway en ARQ-01, y el reparto no es arbitrario:
//
//	1. Cilium/eBPF   por IP y conexión      — barato, tosco, antes de la app
//	2. Valkey        por cuenta y agente    — esto
//	3. PostgreSQL    en la misma transacción — exacto, y el único que decide
//
// Esta capa existe para que el tráfico abusivo no llegue a la tercera. Contar
// cuotas en PostgreSQL funciona —así estaba el límite de envío de códigos hasta
// ahora— pero pone lectura y escritura sobre el primario en cada intento, que
// es justo la carga que el pooler no puede absorber a la escala de RNF-03.

// AlFallar dice qué hacer cuando Valkey no responde.
//
// No hay una respuesta buena para las dos rutas, así que la elige cada una:
//
//	Permitir  para lo que tiene otra capa detrás. Un limitador que tumba el
//	          servicio cuando su almacén parpadea hace más daño que el abuso
//	          que evita, y las capas 1 y 3 siguen ahí.
//
//	Denegar   para lo que NO la tiene. El envío de códigos es el caso: detrás
//	          no hay ninguna otra capa, y el coste de negar es que alguien
//	          espere unos minutos, mientras que el de permitir es correo
//	          ilimitado hacia una dirección que alguien escribió.
type AlFallar int

const (
	Permitir AlFallar = iota
	Denegar
)

// Veredicto es el resultado de consultar una cuota.
type Veredicto struct {
	Permitido bool

	// Espera es cuánto falta para que la ventana se abra otra vez. Va tal cual
	// a la cabecera Retry-After, que el contrato ya declara en
	// DemasiadasPeticiones y que hasta ahora no emitía nadie: sin ella, un
	// cliente educado no tiene forma de saber cuándo reintentar y acaba
	// haciéndolo en bucle, que es lo contrario de lo que el límite busca.
	Espera time.Duration
}

// ventanaFija cuenta e impone la caducidad en una sola ida.
//
// En Lua y no en dos comandos porque INCR y EXPIRE por separado tienen una
// carrera real: si el proceso muere entre los dos —o si el EXPIRE falla— la
// clave se queda sin caducidad y el contador no vuelve a bajar nunca. Ese
// destino queda bloqueado para siempre por un fallo transitorio.
//
// Devuelve el contador y los milisegundos que faltan para que la ventana
// expire, para poder responder Retry-After sin una segunda consulta.
var ventanaFija = valkey.NewLuaScript(`
  local actual = redis.call('INCR', KEYS[1])
  if actual == 1 then
    redis.call('PEXPIRE', KEYS[1], ARGV[1])
  end
  return {actual, redis.call('PTTL', KEYS[1])}
`)

// Permite consulta la cuota de una clave y la consume.
//
// La ventana es fija, no deslizante. Una deslizante es más justa en el borde
// —con la fija se pueden gastar dos ventanas seguidas en un instante— pero
// exige guardar la marca de cada intento en vez de un contador. Para cuotas de
// tres envíos por hora esa precisión no compra nada y multiplica la memoria por
// destino.
func (c *Cliente) Permite(
	ctx context.Context, clave string, maximo int, ventana time.Duration, alFallar AlFallar,
) Veredicto {
	respuesta, err := ventanaFija.Exec(ctx, c.valkey,
		[]string{"limite:" + clave},
		[]string{strconv.FormatInt(ventana.Milliseconds(), 10)},
	).ToArray()
	if err != nil {
		return Veredicto{Permitido: alFallar == Permitir, Espera: ventana}
	}

	actual, err := respuesta[0].AsInt64()
	if err != nil {
		return Veredicto{Permitido: alFallar == Permitir, Espera: ventana}
	}

	if actual <= int64(maximo) {
		return Veredicto{Permitido: true}
	}

	// El PTTL puede venir negativo si la clave caducó entre el INCR y la
	// lectura. Devolver la ventana entera es la respuesta segura: pedir que
	// espere de más es preferible a decirle que reintente ya y que vuelva a
	// rebotar.
	espera := ventana
	if ttl, err := respuesta[1].AsInt64(); err == nil && ttl > 0 {
		espera = time.Duration(ttl) * time.Millisecond
	}

	return Veredicto{Permitido: false, Espera: espera}
}
