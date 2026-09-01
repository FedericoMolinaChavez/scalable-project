// Package identidad es el "Servicio de Identidad" de ARQ-01, acotado a lo que
// RF-02 necesita: que quien reservó como invitado pueda volver a ver lo suyo.
//
// No es autenticación de cuentas. RF-12 —contraseñas, magic link, 2FA, sesiones
// con refresco— es otro requisito y otra superficie; lo único que comparten es
// la tabla token_verificacion y este paquete, que crecerá hacia allí.
//
// Toda la seguridad de esto descansa en cuatro cosas, y ninguna es opcional:
// el código nunca se guarda en claro, se compara en tiempo constante, se quema
// a los tres intentos, y la respuesta es idéntica exista o no el destino.
package identidad

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// PropositoConsulta es el valor de plataforma.proposito_token que usa RF-02.
const PropositoConsulta = "consulta_reservas"

// digitos es la longitud del código, según RF-12 y RF-19.
const digitos = 6

var (
	// ErrCodigoInvalido cubre el código equivocado, el caducado, el ya usado y
	// el que nunca existió. Es uno solo a propósito: cada distinción que se le
	// devuelve a quien está probando códigos le dice si va por buen camino.
	ErrCodigoInvalido = errors.New("el código no es válido o ya caducó")

	// ErrDemasiadosEnvios es el límite de RF-12 A11. Este SÍ se distingue,
	// porque no es un fallo de quien lo recibe: es el sistema pidiéndole que
	// espere, y necesita saberlo para no quedarse pulsando un botón que ya no
	// hace nada.
	ErrDemasiadosEnvios = errors.New("se pidieron demasiados códigos para ese destino")

	// ErrDestinoInvalido es una dirección que no tiene forma de dirección. Se
	// comprueba antes de tocar la base para no llenar la tabla de basura, y no
	// filtra nada: dice que lo escrito no es un correo, no si ese correo existe.
	ErrDestinoInvalido = errors.New("el destino no parece una dirección de correo")
)

// Limitador impone las cuotas de RNF-08. Lo cumple *cache.Cliente.
type Limitador interface {
	Permite(ctx context.Context, clave string, maximo int, ventana time.Duration, alFallar cache.AlFallar) cache.Veredicto
}

// Servicio emite y canjea códigos de un solo uso.
type Servicio struct {
	bd       *datos.BD
	emisor   correo.Emisor
	firmante *Firmante
	limites  Limitador
	registro *slog.Logger

	ttlCodigo     time.Duration
	maxIntentos   int
	maxEnviosHora int
}

func Nuevo(
	bd *datos.BD, emisor correo.Emisor, firmante *Firmante, limites Limitador, registro *slog.Logger,
	ttlCodigo time.Duration, maxIntentos, maxEnviosHora int,
) *Servicio {
	return &Servicio{
		bd:            bd,
		emisor:        emisor,
		firmante:      firmante,
		limites:       limites,
		registro:      registro,
		ttlCodigo:     ttlCodigo,
		maxIntentos:   maxIntentos,
		maxEnviosHora: maxEnviosHora,
	}
}

// Solicitar genera un código, lo guarda con su huella y lo manda por correo.
//
// NO dice si el destino tiene reservas ni si existe una cuenta con él, y esa es
// la propiedad más importante de esta función (RF-12 A12, y la leyenda de
// RF-02). Una respuesta que distinga "te lo mandé" de "esa dirección no está"
// convierte este formulario en un buscador de clientes del negocio: se prueban
// direcciones y las que respondan distinto son las que reservaron.
//
// Por eso solo devuelve error cuando el fallo es del propio sistema o cuando se
// superó el límite de envíos, nunca por lo que valga el destino.
func (s *Servicio) Solicitar(ctx context.Context, destino string) error {
	destino = normalizar(destino)
	if !pareceCorreo(destino) {
		return ErrDestinoInvalido
	}

	codigo, err := generarCodigo()
	if err != nil {
		return err
	}

	// La cuota se consulta en Valkey, que es donde ARQ-01 pone la segunda de
	// las tres capas de RNF-08. Antes se contaban filas de token_verificacion
	// dentro de la propia transacción: correcto, pero ponía lectura y escritura
	// sobre el PRIMARIO en cada intento, que es exactamente la carga que esta
	// capa existe para que no llegue allí.
	//
	// Se consume ANTES de escribir nada. Un intento que rebota no debe dejar
	// rastro en PostgreSQL, o el abuso seguiría costando escrituras aunque se
	// rechace.
	//
	// Falla CERRADO, y es el único sitio del sistema donde eso es lo correcto:
	// detrás de este límite no hay ninguna otra capa —no hay invariante en el
	// motor que impida mandar correo— así que con Valkey caído el coste de
	// negar es que alguien espere, y el de permitir es correo ilimitado hacia
	// una dirección que cualquiera escribe en un formulario.
	if veredicto := s.limites.Permite(
		ctx, "codigo:"+destino, s.maxEnviosHora, time.Hour, cache.Denegar,
	); !veredicto.Permitido {
		return ErrDemasiadosEnvios
	}

	err = s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		// Los códigos anteriores del mismo destino se queman al pedir uno
		// nuevo. Sin esto, pedir tres códigos deja tres puertas abiertas a la
		// vez y triplica lo que puede adivinar quien los esté probando.
		if _, err := tx.Exec(ctx, `
			UPDATE plataforma.token_verificacion
			SET usado_en = now()
			WHERE destino = $1 AND proposito = $2 AND usado_en IS NULL`,
			destino, PropositoConsulta); err != nil {
			return err
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO plataforma.token_verificacion
				(cuenta_id, proposito, canal, destino, valor_hash, expira_en)
			VALUES (NULL, $1, 'email', $2, $3, now() + make_interval(secs => $4))`,
			PropositoConsulta, destino, huella(codigo), s.ttlCodigo.Seconds())
		return err
	})
	if err != nil {
		return err
	}

	// El envío va FUERA de la transacción, y el orden es deliberado: si se
	// enviara dentro y la transacción se revirtiera después, existiría un
	// código en el buzón de alguien que la base no reconoce. Al revés, el peor
	// caso es un código guardado que nunca llegó, y eso se arregla pidiendo
	// otro.
	mensaje := correo.Mensaje{
		Para:   destino,
		Asunto: "Tu código para ver tus reservas",
		Cuerpo: cuerpoCodigo(codigo, s.ttlCodigo),
	}
	if err := s.emisor.Enviar(ctx, mensaje); err != nil {
		return fmt.Errorf("no se pudo enviar el código: %w", err)
	}

	return nil
}

// Canjear comprueba el código y devuelve un token de acceso.
//
// Devuelve también hasta cuándo vale, para que la interfaz pueda avisar antes
// de que caduque en vez de fallar al siguiente clic.
func (s *Servicio) Canjear(ctx context.Context, destino, codigo string) (string, time.Time, error) {
	destino = normalizar(destino)
	codigo = strings.TrimSpace(codigo)

	if !pareceCorreo(destino) || len(codigo) != digitos {
		return "", time.Time{}, ErrCodigoInvalido
	}

	// El resultado del canje se lleva FUERA de la transacción en vez de
	// devolverse como error desde dentro, y no es un rodeo estilístico: es la
	// única forma de que el contador de intentos sobreviva.
	//
	// EnTenant y SinTenant revierten cuando fn devuelve error. Un intento
	// fallido que devolviera ErrCodigoInvalido desde dentro revertiría, junto
	// con el error, el `intentos = intentos + 1` que acababa de escribir. El
	// contador volvería a cero en cada intento y el límite de RF-12 A2 no
	// existiría: un código de seis dígitos se adivinaría por fuerza bruta sin
	// que nada lo frenase.
	//
	// Así que la transacción confirma SIEMPRE —el contador es un hecho que
	// ocurrió— y el veredicto viaja en esta variable.
	var veredicto error

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		veredicto = nil

		var (
			id       string
			hash     string
			intentos int
		)

		// FOR UPDATE: dos canjes simultáneos del mismo código tienen que
		// serializarse, o los dos leen `intentos` a la vez y el contador
		// avanza uno en vez de dos, que es justo como se salta un límite de
		// tres intentos.
		err := tx.QueryRow(ctx, `
			SELECT id::text, valor_hash, intentos
			FROM plataforma.token_verificacion
			WHERE destino = $1
			  AND proposito = $2
			  AND usado_en IS NULL
			  AND expira_en > now()
			ORDER BY creado_en DESC
			LIMIT 1
			FOR UPDATE`,
			destino, PropositoConsulta).Scan(&id, &hash, &intentos)
		if errors.Is(err, pgx.ErrNoRows) {
			veredicto = ErrCodigoInvalido
			return nil
		}
		if err != nil {
			return err
		}

		if intentos >= s.maxIntentos {
			// Ya gastado por fuerza bruta: se quema para que no siga vivo
			// hasta que expire.
			veredicto = ErrCodigoInvalido
			_, err := tx.Exec(ctx,
				"UPDATE plataforma.token_verificacion SET usado_en = now() WHERE id = $1", id)
			return err
		}

		// subtle.ConstantTimeCompare y no ==: comparar huellas con == corta en
		// el primer byte distinto y el tiempo de respuesta filtra cuántos
		// bytes se acertaron.
		if subtle.ConstantTimeCompare([]byte(hash), []byte(huella(codigo))) != 1 {
			veredicto = ErrCodigoInvalido
			_, err := tx.Exec(ctx,
				"UPDATE plataforma.token_verificacion SET intentos = intentos + 1 WHERE id = $1", id)
			return err
		}

		// Un solo uso (ER-02): se quema al acertar, no al caducar.
		_, err = tx.Exec(ctx,
			"UPDATE plataforma.token_verificacion SET usado_en = now() WHERE id = $1", id)
		return err
	})
	if err != nil {
		return "", time.Time{}, err
	}
	if veredicto != nil {
		return "", time.Time{}, veredicto
	}

	return s.firmante.Emitir(destino, time.Now())
}

// Verificar comprueba un token de acceso ya emitido. Lo usan los servicios de
// lectura y de escritura para saber de quién es la petición.
func (s *Servicio) Verificar(token string) (Acceso, error) {
	return s.firmante.Verificar(token, time.Now())
}

// generarCodigo produce seis dígitos con el generador criptográfico.
//
// crypto/rand y no math/rand. Un código de seis dígitos sacado de un generador
// con semilla predecible no es un secreto: quien conozca el instante de
// arranque del proceso puede reproducir la secuencia entera.
func generarCodigo() (string, error) {
	maximo := big.NewInt(1_000_000)

	n, err := rand.Int(rand.Reader, maximo)
	if err != nil {
		return "", fmt.Errorf("no se pudo generar el código: %w", err)
	}

	// Con relleno de ceros: 42 es un código válido y tiene que viajar como
	// "000042", o la comprobación de longitud lo rechazaría al volver.
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// huella es lo único que se guarda del código (RNF-09).
//
// SHA-256 a secas, sin sal ni derivación lenta, y aquí eso es correcto: no es
// una contraseña. Un código de seis dígitos vive cinco minutos y aguanta tres
// intentos, así que la defensa es el espacio pequeño acotado por el tiempo y
// por el contador, no la lentitud del hash. Lo que compra el hash es que
// alguien con acceso de lectura a la tabla no pueda usar los códigos que ve.
func huella(codigo string) string {
	suma := sha256.Sum256([]byte(codigo))
	return hex.EncodeToString(suma[:])
}

// normalizar deja el destino comparable: sin espacios y en minúsculas.
//
// Quien escribió Ana@Ejemplo.com al reservar teclea ana@ejemplo.com al volver, y
// es la misma persona. La parte local de una dirección es técnicamente sensible
// a mayúsculas según el RFC, pero ningún proveedor real lo aplica, y tratarlas
// como distintas aquí solo produce gente que no puede ver sus propias reservas.
func normalizar(destino string) string {
	return strings.ToLower(strings.TrimSpace(destino))
}

// pareceCorreo es una comprobación de forma, no de existencia.
func pareceCorreo(destino string) bool {
	arroba := strings.IndexByte(destino, '@')
	if arroba <= 0 || arroba == len(destino)-1 {
		return false
	}
	if strings.Count(destino, "@") != 1 {
		return false
	}
	if len(destino) > 254 {
		return false
	}
	return strings.Contains(destino[arroba+1:], ".")
}

func cuerpoCodigo(codigo string, vigencia time.Duration) string {
	return fmt.Sprintf(
		"Tu código para ver tus reservas es:\n\n    %s\n\n"+
			"Caduca en %d minutos y solo sirve una vez.\n\n"+
			"Si no lo pediste tú, no hace falta que hagas nada: sin el código nadie\n"+
			"puede ver nada.\n",
		codigo, int(vigencia.Minutes()))
}
