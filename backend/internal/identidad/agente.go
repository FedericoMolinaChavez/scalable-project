package identidad

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
)

// ErrAgenteNoAutorizado es la única respuesta de un intercambio que no sale.
//
// Cubre las tres razones de RF-13 —credencial que no vale, cuenta que no
// autorizó a este agente (RNF-07), acciones fuera del alcance de esa cuenta
// (RF-23)— y las cubre juntas a propósito: separarlas le diría a quien prueba
// credenciales cuál de los tres pasos ya superó, que es la mitad del trabajo
// hecho.
var ErrAgenteNoAutorizado = errors.New("el agente no puede realizar esas acciones sobre esa cuenta")

// ErrDemasiadasAcciones es el límite de RF-13: "más de 3-4 iguales en 1
// minuto". Se distingue del rechazo porque no es una falta de permiso: es el
// sistema pidiendo que espere, y un agente que no sepa distinguirlas
// reintentaría un permiso que sí tiene.
var ErrDemasiadasAcciones = errors.New("demasiados intercambios seguidos para esa cuenta")

// maxIntercambiosMinuto es el número de RF-13. Cuatro y no tres: el diagrama
// dice "3-4" y el borde superior deja pasar el caso legítimo de un agente que
// atiende a alguien por teléfono, consulta, corrige y vuelve a pedir.
const maxIntercambiosMinuto = 4

// alcancePorTipo es RF-23 escrito como lo que es: una tabla de qué puede hacer
// cada tipo de cuenta.
//
// Un agente NUNCA aparece aquí, y eso es lo importante: no tiene alcance propio.
// Lo que recibe es la INTERSECCIÓN de lo que pide con lo que ya puede la cuenta
// a la que representa, así que esta tabla se consulta por el tipo de la cuenta
// impersonada, nunca por el agente.
var alcancePorTipo = map[string][]api.AccionAgente{
	TipoUsuario: {
		api.Reservar,        // RF-04
		api.ListarReservas,  // RF-05
		api.CancelarReserva, // RF-06 en su nombre
	},
	TipoAdmin: {
		api.Reservar,
		api.ListarReservas,
		api.CancelarReserva,
	},
	TipoSuperAdmin: {
		api.Reservar,
		api.ListarReservas,
		api.CancelarReserva,
	},
}

// IntercambiarTokenAgente emite el token de acciones de RF-13.
//
// El token que sale acredita al AGENTE actuando por una CUENTA, con un alcance
// ya recortado. Nada de lo que venga después puede ampliarlo: el alcance viaja
// firmado dentro del token, así que ni el agente ni ninguna ruta pueden añadirle
// una acción que la cuenta no tuviera.
func (s *Servicio) IntercambiarTokenAgente(
	ctx context.Context, credencial string, cuentaID uuid.UUID, acciones []api.AccionAgente,
) (api.TokenAgente, error) {
	if credencial == "" || len(acciones) == 0 {
		return api.TokenAgente{}, ErrAgenteNoAutorizado
	}

	var (
		agenteID  string
		tipo      string
		tenantID  *string
		correo    *string
		concedido []api.AccionAgente
		expira    time.Time
	)

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		// Las tres comprobaciones de RF-13, en una consulta. El JOIN con
		// autorizacion_agente es RNF-07: un agente registrado pero sin permiso
		// de ESTA cuenta no encuentra fila, exactamente igual que uno cuya
		// credencial no existe.
		err := tx.QueryRow(ctx, `
			SELECT a.id::text, c.tipo::text, c.tenant_id::text, c.email
			FROM plataforma.agente a
			JOIN plataforma.autorizacion_agente au
			  ON au.agente_id = a.id AND au.cuenta_id = $2 AND au.revocada_en IS NULL
			JOIN plataforma.cuenta c
			  ON c.id = au.cuenta_id AND c.estado = 'activa'
			WHERE a.credencial_hash = $1 AND a.estado = 'activo'`,
			huella(credencial), cuentaID).Scan(&agenteID, &tipo, &tenantID, &correo)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAgenteNoAutorizado
		}
		if err != nil {
			return err
		}

		concedido = intersecar(acciones, alcancePorTipo[tipo])
		if len(concedido) == 0 {
			return ErrAgenteNoAutorizado
		}

		// La fila es el registro del permiso, no el secreto: lo que viaja es un
		// token firmado. Existe para que la auditoría de RF-36 pueda responder
		// qué se concedió a quién y cuándo, sin tener que descifrar tokens
		// guardados en registros.
		return tx.QueryRow(ctx, `
			INSERT INTO plataforma.token_agente
				(agente_id, cuenta_impersonada_id, alcance, expira_en)
			VALUES ($1, $2, $3::jsonb, now() + make_interval(secs => $4))
			RETURNING expira_en`,
			agenteID, cuentaID, jsonDeAcciones(concedido), s.firmante.Vigencia().Seconds(),
		).Scan(&expira)
	})
	if err != nil {
		return api.TokenAgente{}, err
	}

	// El límite se consulta con el agente YA identificado, después de la
	// consulta y antes de firmar. Contarlo por credencial escrita dejaría que
	// probar credenciales inexistentes llenara claves en Valkey sin límite;
	// contarlo por agente y cuenta es lo que RF-13 describe.
	//
	// Falla ABIERTO, al revés que el de contraseñas, y la diferencia es que
	// aquí SÍ hay otra capa detrás: el agente ya demostró su credencial y su
	// autorización sobre esa cuenta, así que lo que este límite acota es el
	// volumen, no el acceso.
	if v := s.limites.Permite(
		ctx, "agente:"+agenteID+":"+cuentaID.String(),
		maxIntercambiosMinuto, time.Minute, cache.Permitir,
	); !v.Permitido {
		return api.TokenAgente{}, ErrDemasiadasAcciones
	}

	alcanceTexto := make([]string, 0, len(concedido))
	for _, a := range concedido {
		alcanceTexto = append(alcanceTexto, string(a))
	}

	token, expiraToken, err := s.firmante.EmitirAcceso(Acceso{
		Destino: valor(correo),
		Cuenta:  cuentaID.String(),
		Tipo:    tipo,
		Tenant:  valor(tenantID),
		Agente:  agenteID,
		Alcance: alcanceTexto,
	}, time.Now())
	if err != nil {
		return api.TokenAgente{}, err
	}

	impersonada := cuentaID
	return api.TokenAgente{
		Token:               token,
		ExpiraEn:            expiraToken,
		Alcance:             concedido,
		CuentaImpersonadaId: &impersonada,
	}, nil
}

// intersecar es RF-23 en una función: lo pedido ∩ lo que la cuenta ya puede.
//
// Devuelve el subconjunto y no un error cuando algo queda fuera, y esa
// distinción es la que hace utilizable el flujo de RF-04: un agente que pide
// tres acciones y solo tiene dos recibe las dos y sabe cuáles son, en vez de un
// rechazo entero que le obliga a adivinar cuál sobraba delante de la persona a
// la que está atendiendo.
func intersecar(pedidas, permitidas []api.AccionAgente) []api.AccionAgente {
	concedidas := make([]api.AccionAgente, 0, len(pedidas))

	for _, pedida := range pedidas {
		if slices.Contains(permitidas, pedida) && !slices.Contains(concedidas, pedida) {
			concedidas = append(concedidas, pedida)
		}
	}

	return concedidas
}

// jsonDeAcciones serializa el alcance para la columna jsonb.
//
// A mano y no con encoding/json porque el valor es una lista de constantes de
// un enum cerrado: no hay nada que escapar que no venga ya validado por el
// contrato, y el paso por un []byte intermedio solo añadiría un error que no
// puede ocurrir.
func jsonDeAcciones(acciones []api.AccionAgente) string {
	salida := "["
	for i, a := range acciones {
		if i > 0 {
			salida += ","
		}
		salida += `"` + string(a) + `"`
	}
	return salida + "]"
}
