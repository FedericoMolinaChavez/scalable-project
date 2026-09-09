package identidad_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/pruebas"
)

// registrarAgente da de alta un agente y, opcionalmente, su autorización sobre
// una cuenta (RNF-07).
//
// Las dos cosas van separadas a propósito porque son dos permisos distintos:
// "este agente existe" y "esta persona le dejó hablar por ella". La prueba de
// que un agente registrado pero sin autorización no puede hacer nada necesita
// poder construir exactamente ese estado.
func registrarAgente(t *testing.T, cuentaID string, autorizar bool) string {
	t.Helper()

	bd := pruebas.AbrirBD(t)
	credencial := "cred-" + uuid.NewString() + "-" + uuid.NewString()
	suma := sha256.Sum256([]byte(credencial))

	err := bd.SinTenant(t.Context(), func(tx pgx.Tx) error {
		var agenteID string
		if err := tx.QueryRow(t.Context(), `
			INSERT INTO plataforma.agente (nombre, credencial_hash)
			VALUES ($1, $2)
			RETURNING id::text`,
			"agente de prueba", hex.EncodeToString(suma[:])).Scan(&agenteID); err != nil {
			return err
		}

		if !autorizar {
			return nil
		}

		_, err := tx.Exec(t.Context(), `
			INSERT INTO plataforma.autorizacion_agente (cuenta_id, agente_id)
			VALUES ($1, $2)`, cuentaID, agenteID)
		return err
	})
	if err != nil {
		t.Fatalf("no se pudo registrar el agente: %v", err)
	}

	return credencial
}

// ------------------------------------------------------------------ RF-13 --

// El alcance que sale es la INTERSECCIÓN con lo que la cuenta ya puede (RF-23),
// no lo que el agente pida. Y viaja en la respuesta porque puede ser menor: sin
// eso, la única forma de descubrir que una acción quedó fuera es intentarla
// delante de la persona a la que se está atendiendo.
func TestElTokenDeAgenteLlevaElAlcanceRecortado(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))
	credencial := registrarAgente(t, cuenta.Id.String(), true)

	token, err := svc.IntercambiarTokenAgente(t.Context(), credencial, cuenta.Id,
		[]api.AccionAgente{api.ListarReservas})
	if err != nil {
		t.Fatalf("IntercambiarTokenAgente devolvió error: %v", err)
	}

	if len(token.Alcance) != 1 || token.Alcance[0] != api.ListarReservas {
		t.Fatalf("el alcance concedido es %v", token.Alcance)
	}
	if token.CuentaImpersonadaId == nil || *token.CuentaImpersonadaId != cuenta.Id {
		t.Fatalf("el token no dice sobre qué cuenta actúa: %v", token.CuentaImpersonadaId)
	}

	firmante := identidad.NuevoFirmante([]byte(secreto), 0)
	acceso, err := firmante.Verificar(token.Token, tiempoDentroDeLaVigencia(token))
	if err != nil {
		t.Fatalf("el token de agente no se puede verificar: %v", err)
	}

	if !acceso.PorAgente() {
		t.Fatal("el token no se identifica como de agente")
	}
	if acceso.Cuenta != cuenta.Id.String() {
		t.Fatalf("el token actúa sobre la cuenta %q y se pidió %q", acceso.Cuenta, cuenta.Id)
	}

	// El alcance ACOTA: la acción concedida pasa y cualquier otra no, aunque el
	// tipo de la cuenta impersonada sí la tuviera. Es lo que hace que un token
	// emitido para consultar no sirva para reservar.
	if !acceso.Permite(string(api.ListarReservas)) {
		t.Fatal("el token no permite la acción que se le concedió")
	}
	if acceso.Permite(string(api.Reservar)) {
		t.Fatal("el token permite una acción que no se le concedió")
	}
	if acceso.Permite(string(api.CancelarReserva)) {
		t.Fatal("el token permite cancelar sin habérselo concedido")
	}
}

// Las tres razones de rechazo de RF-13 responden lo mismo: separarlas le diría
// a quien prueba credenciales qué parte del camino ya superó.
func TestLasTresRazonesDeRechazoDelAgenteSonIndistinguibles(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))
	otra := registrada(t, svc, buzon, destinoUnico(t))

	autorizado := registrarAgente(t, cuenta.Id.String(), true)
	sinAutorizar := registrarAgente(t, cuenta.Id.String(), false)

	casos := []struct {
		nombre     string
		credencial string
		cuenta     uuid.UUID
	}{
		{"credencial que no existe", "cred-inventada-que-no-esta-en-la-tabla", cuenta.Id},
		{"agente sin autorización de esa cuenta", sinAutorizar, cuenta.Id},
		{"agente autorizado sobre OTRA cuenta", autorizado, otra.Id},
	}

	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			_, err := svc.IntercambiarTokenAgente(t.Context(), caso.credencial, caso.cuenta,
				[]api.AccionAgente{api.ListarReservas})
			if !errors.Is(err, identidad.ErrAgenteNoAutorizado) {
				t.Fatalf("se esperaba ErrAgenteNoAutorizado y salió %v", err)
			}
		})
	}
}

// Un agente que pide más de lo que la cuenta puede recibe el subconjunto, no un
// rechazo. Con las tres acciones actuales y una cuenta de tipo usuario, la
// intersección es total; lo que la prueba fija es que el resultado sea un
// subconjunto de lo pedido y nunca lo contrario.
func TestElAgenteNuncaRecibeMasDeLoQuePide(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))
	credencial := registrarAgente(t, cuenta.Id.String(), true)

	pedidas := []api.AccionAgente{api.Reservar}

	token, err := svc.IntercambiarTokenAgente(t.Context(), credencial, cuenta.Id, pedidas)
	if err != nil {
		t.Fatalf("IntercambiarTokenAgente devolvió error: %v", err)
	}

	for _, concedida := range token.Alcance {
		if !slices.Contains(pedidas, concedida) {
			t.Fatalf("se concedió %q sin haberla pedido", concedida)
		}
	}
}

func TestUnIntercambioSinAccionesNoEmiteNada(t *testing.T) {
	svc, buzon := servicioCuentas(t)
	cuenta := registrada(t, svc, buzon, destinoUnico(t))
	credencial := registrarAgente(t, cuenta.Id.String(), true)

	// Un token con alcance vacío no es un caso degenerado inofensivo: pasaría
	// la comprobación de firma y fallaría en cada acción, convirtiendo un error
	// de emisión en uno de uso mucho más lejos de su causa.
	if _, err := svc.IntercambiarTokenAgente(
		t.Context(), credencial, cuenta.Id, nil,
	); !errors.Is(err, identidad.ErrAgenteNoAutorizado) {
		t.Fatalf("se emitió un token sin alcance: %v", err)
	}
}

// tiempoDentroDeLaVigencia da un instante en el que el token todavía vale.
//
// Hace falta porque el firmante de la prueba se construye solo para verificar
// —su vigencia no se usa, la del token ya está firmada dentro— y verificar
// exige un "ahora" con el que comparar.
func tiempoDentroDeLaVigencia(token api.TokenAgente) time.Time {
	return token.ExpiraEn.Add(-time.Second)
}
