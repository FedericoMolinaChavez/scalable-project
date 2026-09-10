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

// Consumo lee el contador de una clave SIN consumirlo.
//
// Hace falta para el bloqueo por intentos fallidos de RF-12 A3/A4, que cuenta
// una cosa distinta de lo que cuenta Permite. Permite cuenta INTENTOS: cada
// llamada gasta cuota, que es lo correcto para "tres códigos por hora". El
// bloqueo de login cuenta FALLOS CONSECUTIVOS, así que un inicio de sesión
// correcto no puede gastar nada —si lo hiciera, cinco entradas legítimas
// seguidas bloquearían la cuenta— y hay que poder preguntar por el contador
// antes de saber si este intento va a fallar.
//
// El tercer valor dice si se pudo consultar. Distinguirlo de "cero fallos" es
// lo que permite a quien llama elegir su política de fallo en vez de heredar
// la de aquí, que es la misma libertad que da AlFallar en Permite.
func (c *Cliente) Consumo(ctx context.Context, clave string) (int, time.Duration, bool) {
	respuesta := c.valkey.DoMulti(ctx,
		c.valkey.B().Get().Key("limite:"+clave).Build(),
		c.valkey.B().Pttl().Key("limite:"+clave).Build(),
	)

	actual, errGet := respuesta[0].AsInt64()
	if errGet != nil {
		// Nil significa que la clave no está: cero fallos acumulados, y eso SÍ
		// es una respuesta, no un fallo de consulta.
		if valkey.IsValkeyNil(errGet) {
			return 0, 0, true
		}
		return 0, 0, false
	}

	var espera time.Duration
	if ttl, err := respuesta[1].AsInt64(); err == nil && ttl > 0 {
		espera = time.Duration(ttl) * time.Millisecond
	}

	return int(actual), espera, true
}

// Reiniciar borra el contador de una clave.
//
// Es la otra mitad de "fallos CONSECUTIVOS": sin esto, cuatro fallos repartidos
// a lo largo de una hora y un quinto al final bloquearían una cuenta que en
// realidad se usó bien cuatro veces en medio.
func (c *Cliente) Reiniciar(ctx context.Context, clave string) {
	// El error se ignora a propósito y es el único sitio del paquete donde eso
	// es correcto: no poder borrar el contador solo significa que un bloqueo
	// caducará por tiempo en vez de al acertar. Propagarlo convertiría un fallo
	// del caché en un fallo de inicio de sesión, que es exactamente lo que la
	// nota de ARQ-01 prohíbe.
	_ = c.valkey.Do(ctx, c.valkey.B().Del().Key("limite:"+clave).Build()).Error()
}
