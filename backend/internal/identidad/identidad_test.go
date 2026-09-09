package identidad_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// El canje sí toca la base: el código vive en plataforma.token_verificacion con
// su vigencia, su contador de intentos y su marca de uso, y todo lo que estas
// pruebas comprueban ocurre allí.
//
// El correo, en cambio, se sustituye. No hay nada que verificar en el envío que
// no verifique ya el propio Mailpit, y hacer que las pruebas dependan de un
// SMTP levantado las volvería lentas y frágiles a cambio de nada. Lo que sí
// importa —qué código se generó— se lee del buzón simulado.

// buzon captura lo enviado en vez de mandarlo.
type buzon struct {
	mu       sync.Mutex
	mensajes []correo.Mensaje
	fallo    error
}

func (b *buzon) Enviar(_ context.Context, mensaje correo.Mensaje) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.fallo != nil {
		return b.fallo
	}
	b.mensajes = append(b.mensajes, mensaje)
	return nil
}

// ultimoCodigo saca los seis dígitos del último correo.
func (b *buzon) ultimoCodigo(t *testing.T) string {
	t.Helper()

	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.mensajes) == 0 {
		t.Fatal("no se envió ningún correo")
	}

	encontrado := regexp.MustCompile(`\b(\d{6})\b`).
		FindStringSubmatch(b.mensajes[len(b.mensajes)-1].Cuerpo)
	if encontrado == nil {
		t.Fatalf("el correo no lleva un código de seis dígitos: %q", b.mensajes[len(b.mensajes)-1].Cuerpo)
	}
	return encontrado[1]
}

func (b *buzon) enviados() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.mensajes)
}

func servicio(t *testing.T, ttl time.Duration, maxIntentos, maxEnvios int) (*identidad.Servicio, *buzon) {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	b := &buzon{}
	registro := slog.New(slog.NewTextHandler(io.Discard, nil))

	return identidad.Nuevo(
		bd, b, identidad.NuevoFirmante([]byte(secreto), 15*time.Minute),
		limitador(t), registro,
		identidad.Opciones{TTLCodigo: ttl, MaxIntentos: maxIntentos, MaxEnviosHora: maxEnvios},
	), b
}

// destinoUnico da una dirección nueva a cada prueba y a cada ejecución.
//
// Las dos cosas hacen falta y por motivos distintos. Entre pruebas, porque el
// límite de envíos es POR DESTINO y compartirlo haría que una fallara por lo
// que hizo otra. Entre ejecuciones, porque ese límite mira la última hora y no
// se puede limpiar: token_verificacion no admite DELETE ni para la aplicación
// —un token gastado es evidencia de un intento de acceso (RNF-36)—, así que la
// segunda pasada del día heredaría los envíos de la primera.
//
// En minúsculas porque el servicio normaliza el destino al guardarlo, y una
// prueba que luego busque la fila por el valor sin normalizar no encuentra
// nada y pasa sin comprobar lo que creía.
func destinoUnico(t *testing.T) string {
	t.Helper()

	nombre := strings.ToLower(regexp.MustCompile(`\W`).ReplaceAllString(t.Name(), "-"))
	return "prb-" + nombre + "-" + uuid.NewString()[:8] + "@ejemplo.test"
}

func TestElCodigoLlegaYSeCanjeaUnaSolaVez(t *testing.T) {
	svc, buzon := servicio(t, 5*time.Minute, 3, 3)
	destino := destinoUnico(t)

	if err := svc.Solicitar(t.Context(), destino); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}

	codigo := buzon.ultimoCodigo(t)

	token, expira, err := svc.Canjear(t.Context(), destino, codigo)
	if err != nil {
		t.Fatalf("Canjear devolvió error: %v", err)
	}
	if token == "" || !expira.After(time.Now()) {
		t.Fatalf("el canje no devolvió un token vigente: %q hasta %s", token, expira)
	}

	// Un solo uso (ER-02). Sin esto, un código filtrado —en un correo
	// reenviado, en un buzón compartido— sirve para siempre.
	if _, _, err := svc.Canjear(t.Context(), destino, codigo); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el código se pudo canjear dos veces: %v", err)
	}
}

// El código se compara sin distinguir mayúsculas en el destino: quien escribió
// Ana@Ejemplo.test al reservar teclea ana@ejemplo.test al volver.
func TestElDestinoNoDistingueMayusculas(t *testing.T) {
	svc, buzon := servicio(t, 5*time.Minute, 3, 3)
	destino := destinoUnico(t)

	if err := svc.Solicitar(t.Context(), "  "+destino+"  "); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}

	if _, _, err := svc.Canjear(t.Context(), upper(destino), buzon.ultimoCodigo(t)); err != nil {
		t.Fatalf("el canje debía aceptar otra capitalización: %v", err)
	}
}

// RF-12 A2: tres intentos y el código se quema. Sin contador, un código de seis
// dígitos son un millón de combinaciones, que no es un número grande para una
// máquina.
func TestElCodigoSeQuemaAlAgotarLosIntentos(t *testing.T) {
	svc, buzon := servicio(t, 5*time.Minute, 3, 3)
	destino := destinoUnico(t)

	if err := svc.Solicitar(t.Context(), destino); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}
	correcto := buzon.ultimoCodigo(t)

	for i := range 3 {
		if _, _, err := svc.Canjear(t.Context(), destino, "000000"); !errors.Is(err, identidad.ErrCodigoInvalido) {
			t.Fatalf("intento %d: se esperaba ErrCodigoInvalido, se obtuvo %v", i+1, err)
		}
	}

	// Y ahora ni siquiera el correcto vale: el código está quemado, no
	// simplemente bloqueado un rato.
	if _, _, err := svc.Canjear(t.Context(), destino, correcto); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el código seguía vivo tras agotar los intentos: %v", err)
	}
}

// Pedir un código nuevo invalida el anterior. Tres códigos vivos a la vez
// triplicarían lo que puede adivinar quien los esté probando.
func TestPedirOtroCodigoInvalidaElAnterior(t *testing.T) {
	svc, buzon := servicio(t, 5*time.Minute, 3, 3)
	destino := destinoUnico(t)

	if err := svc.Solicitar(t.Context(), destino); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}
	primero := buzon.ultimoCodigo(t)

	if err := svc.Solicitar(t.Context(), destino); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}
	segundo := buzon.ultimoCodigo(t)

	if primero == segundo {
		t.Fatal("los dos códigos salieron iguales; el generador no está produciendo valores distintos")
	}

	if _, _, err := svc.Canjear(t.Context(), destino, primero); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("el código viejo seguía valiendo: %v", err)
	}
	if _, _, err := svc.Canjear(t.Context(), destino, segundo); err != nil {
		t.Fatalf("el código nuevo no valía: %v", err)
	}
}

// RF-12 A11: sin este límite, el formulario de "enviarme un código" es un cañón
// de correo apuntando a la dirección que alguien escriba.
func TestElLimiteDeEnviosPorHora(t *testing.T) {
	svc, buzon := servicio(t, 5*time.Minute, 3, 2)
	destino := destinoUnico(t)

	for i := range 2 {
		if err := svc.Solicitar(t.Context(), destino); err != nil {
			t.Fatalf("envío %d: %v", i+1, err)
		}
	}

	err := svc.Solicitar(t.Context(), destino)
	if !errors.Is(err, identidad.ErrDemasiadosEnvios) {
		t.Fatalf("se esperaba ErrDemasiadosEnvios, se obtuvo %v", err)
	}

	// Y el tercero no llegó a mandarse: el límite corta antes del envío, no
	// después.
	if n := buzon.enviados(); n != 2 {
		t.Fatalf("se enviaron %d correos y el límite era 2", n)
	}
}

// Un código caducado no vale, aunque sea el correcto y nadie lo haya usado.
//
// Se emite normal y se le adelanta el reloj por SQL. No se puede sembrar ya
// caducado: el esquema lo prohíbe con token_vigencia_valida (expira_en >
// creado_en), y esa restricción es correcta —un código que nace muerto es un
// error de programación, no un caso de uso—, así que la prueba la respeta en
// vez de esquivarla.
func TestUnCodigoCaducadoNoVale(t *testing.T) {
	bd := pruebas.AbrirBD(t)
	b := &buzon{}
	svc := identidad.Nuevo(
		bd, b, identidad.NuevoFirmante([]byte(secreto), 15*time.Minute),
		limitador(t), slog.New(slog.NewTextHandler(io.Discard, nil)),
		identidad.Opciones{TTLCodigo: 5 * time.Minute, MaxIntentos: 3, MaxEnviosHora: 3})

	destino := destinoUnico(t)

	if err := svc.Solicitar(t.Context(), destino); err != nil {
		t.Fatalf("Solicitar devolvió error: %v", err)
	}
	codigo := b.ultimoCodigo(t)

	// Se retrasan las DOS fechas, no solo la de caducidad. token_vigencia_valida
	// exige expira_en > creado_en y también vigila los UPDATE, así que un token
	// no puede caducar hacia atrás: eso es correcto, un código que caduca antes
	// de existir no significa nada. Lo que sí existe es un código viejo, y un
	// código viejo tiene las dos fechas viejas.
	if err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		etiqueta, err := tx.Exec(t.Context(), `
			UPDATE plataforma.token_verificacion
			SET creado_en = now() - interval '1 hour',
			    expira_en = now() - interval '1 second'
			WHERE destino = $1 AND usado_en IS NULL`, destino)
		if err != nil {
			return err
		}
		if etiqueta.RowsAffected() != 1 {
			t.Fatalf("se caducaron %d filas; se esperaba 1", etiqueta.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("no se pudo caducar el código: %v", err)
	}

	if _, _, err := svc.Canjear(t.Context(), destino, codigo); !errors.Is(err, identidad.ErrCodigoInvalido) {
		t.Fatalf("un código caducado se canjeó: %v", err)
	}
}

// Un destino que no tiene forma de correo se rechaza antes de tocar la base, y
// eso NO filtra nada: dice que lo escrito no es una dirección, no si esa
// dirección existe.
func TestUnDestinoMalFormadoSeRechaza(t *testing.T) {
	svc, _ := servicio(t, 5*time.Minute, 3, 3)

	for _, malo := range []string{"", "   ", "sin-arroba", "@ejemplo.test", "ana@", "ana@sinpunto", "a@b@c.test"} {
		t.Run(malo, func(t *testing.T) {
			if err := svc.Solicitar(t.Context(), malo); !errors.Is(err, identidad.ErrDestinoInvalido) {
				t.Fatalf("se esperaba ErrDestinoInvalido, se obtuvo %v", err)
			}
		})
	}
}

// upper cambia la capitalización de la primera letra, para comprobar que el
// destino no distingue mayúsculas.
func upper(s string) string {
	return regexp.MustCompile(`^.`).ReplaceAllStringFunc(s, func(c string) string {
		return string(c[0] - 32)
	})
}
