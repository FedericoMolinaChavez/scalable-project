// Package auditoria escribe la traza de RF-36.
//
// Lo único que hace es un INSERT, y aun así es un paquete propio por una razón
// que gobierna todo lo demás: RNF-36 dice que la auditoría es **condición de
// éxito** de las acciones críticas. No es un registro que se manda a un sitio,
// es una fila que tiene que caber en la MISMA transacción que la operación. Si
// no cabe, la operación no ocurrió.
//
// De ahí sale la firma: Escribir recibe una pgx.Tx y no una *datos.BD. No puede
// abrir su propia transacción —sería otra, y otra transacción confirma o
// revierte por su cuenta— y no puede fallar silenciosamente, porque su fallo ES
// el fallo de la operación.
//
// Por eso tampoco hay outbox ni relay aquí, al revés que con los eventos de
// negocio: un relay entrega DESPUÉS del COMMIT, y "después" y "condición de
// éxito" son incompatibles. La nota de ARQ-01 lo dice con todas las letras: al
// vivir la auditoría en PostgreSQL, se escribe dentro de la misma transacción.
package auditoria

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/identidad"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/transporte"
)

// Resultados posibles de una acción auditada.
//
// El rechazo también se audita, y no es un detalle de completitud: una traza
// que solo registra lo que salió bien no sirve para investigar nada, porque un
// ataque es exactamente una sucesión de intentos que fallaron.
const (
	Exito   = "exito"
	Rechazo = "rechazo"
)

// Los tipos de recurso sobre los que se audita. Son constantes y no cadenas
// sueltas en cada llamada porque la consulta de RF-36 filtra por este campo:
// con "sede" en un sitio y "Sede" en otro, el filtro deja de encontrar la mitad
// de los eventos y nadie lo nota hasta que hace falta.
const (
	RecursoSede      = "sede"
	RecursoServicio  = "servicio"
	RecursoRecurso   = "recurso"
	RecursoRegla     = "regla_disponibilidad"
	RecursoExcepcion = "excepcion_calendario"
	RecursoPolitica  = "politica_version"
	RecursoVoucher   = "voucher"
	RecursoTarifa    = "tarifa"
	RecursoReserva   = "reserva"
)

// Actor es quién ejecuta la acción, ya resuelto desde el token.
//
// Se separa del Evento porque es lo que NO cambia entre las varias acciones de
// una misma petición: quien construye un Evento por operación no debería tener
// que volver a derivar la identidad cada vez.
type Actor struct {
	// Tipo es un valor de negocio.actor_tipo. Vacío se trata como "sistema".
	Tipo string

	// ID es la cuenta que actúa. Vacío solo en el sistema: lo exige
	// auditoria_actor_coherente, con el mismo criterio que transicion_estado.
	ID string

	// Agente y CuentaImpersonada van juntos o no van (RF-13). Un agente sin
	// cuenta impersonada no es un agente, es un actor sin sujeto.
	Agente            string
	CuentaImpersonada string

	// Alcance son las acciones que el token de agente autorizaba. Se guarda tal
	// cual porque RF-36 pide registrar "el alcance del token utilizado": sin
	// él, la traza dice que un agente hizo algo pero no con qué permiso.
	Alcance []string

	IP          string
	Dispositivo string
}

// DeContexto arma el actor a partir del token verificado y de la petición.
//
// Devuelve un actor de sistema cuando no hay token, y eso es correcto y no un
// caso degenerado: las acciones sin identidad las hacen los trabajadores
// asíncronos, y "sistema" es exactamente lo que son.
func DeContexto(ctx context.Context) Actor {
	cliente := transporte.DeCliente(ctx)

	acceso, hay := identidad.DeAcceso(ctx)
	if !hay || acceso.EsInvitado() {
		// Un invitado tiene token pero no cuenta, así que no hay actor_id que
		// poner y auditoria_actor_coherente exige que solo el sistema lo tenga
		// nulo. Se registra como sistema: la alternativa —inventar un
		// identificador— sería peor que admitir que no hay ninguno.
		return Actor{Tipo: "sistema", IP: cliente.IP, Dispositivo: cliente.Dispositivo}
	}

	actor := Actor{
		Tipo:        tipoDeCuenta(acceso.Tipo),
		ID:          acceso.Cuenta,
		IP:          cliente.IP,
		Dispositivo: cliente.Dispositivo,
	}

	if acceso.PorAgente() {
		// El actor pasa a ser el agente: la traza responde QUIÉN EJECUTÓ, y en
		// nombre de quién va aparte, que es justo lo que RF-36 pide distinguir.
		actor.Tipo = "agente"
		actor.Agente = acceso.Agente
		actor.CuentaImpersonada = acceso.Cuenta
		actor.Alcance = acceso.Alcance
	}

	return actor
}

// tipoDeCuenta traduce el tipo del token al enum de negocio.actor_tipo.
//
// Los dos enums coinciden en los tres valores que comparten, y aun así se
// traduce explícitamente: son dos enums de dos esquemas distintos, y dar por
// hecho que seguirán coincidiendo es la clase de suposición que deja de ser
// cierta sin que nada falle.
func tipoDeCuenta(tipo string) string {
	switch tipo {
	case identidad.TipoAdmin:
		return "administrador"
	case identidad.TipoSuperAdmin:
		return "super_admin"
	case identidad.TipoUsuario:
		return "usuario"
	default:
		return "sistema"
	}
}

// Evento es una acción concreta sobre un recurso concreto.
type Evento struct {
	Accion      string
	RecursoTipo string

	// RecursoID es opcional: una acción rechazada puede no haber llegado a
	// tener un identificador, y un listado no es sobre una fila concreta.
	RecursoID string

	Resultado string
}

// Escribir inserta el evento DENTRO de la transacción que se le pasa.
//
// No abre transacción propia y no puede: sería otra, con su propio COMMIT, y
// entonces la auditoría podría sobrevivir a una operación revertida o al revés.
// Que el parámetro sea una pgx.Tx y no una *datos.BD hace imposible llamarlo
// mal.
func Escribir(ctx context.Context, tx pgx.Tx, tenant uuid.UUID, actor Actor, evento Evento) error {
	if actor.Tipo == "" {
		actor.Tipo = "sistema"
	}
	if actor.Tipo == "sistema" {
		// auditoria_actor_coherente: solo el sistema puede no tener actor_id, y
		// el sistema no puede tenerlo. Se normaliza aquí en vez de dejar que el
		// motor rechace la fila, porque ese rechazo abortaría la operación de
		// negocio por un error de quien construyó el actor.
		actor.ID = ""
	}
	if evento.Resultado == "" {
		evento.Resultado = Exito
	}

	// El jsonb viaja como *string: nil para dejar la columna en NULL, y una
	// cadena —no un []byte— cuando hay algo. pgx manda un []byte en binario y
	// PostgreSQL lo recibe como bytea, así que el cast a jsonb falla con un
	// 22P02 sobre un JSON que era válido.
	var alcance *string
	if len(actor.Alcance) > 0 {
		crudo, err := json.Marshal(actor.Alcance)
		if err != nil {
			return fmt.Errorf("no se pudo serializar el alcance del token: %w", err)
		}
		texto := string(crudo)
		alcance = &texto
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO negocio.evento_auditoria (
			tenant_id, actor_tipo, actor_id,
			agente_id, cuenta_impersonada_id, alcance_token,
			accion, recurso_tipo, recurso_id, resultado, ip, dispositivo
		) VALUES (
			$1, $2::negocio.actor_tipo, NULLIF($3, '')::uuid,
			NULLIF($4, '')::uuid, NULLIF($5, '')::uuid, $6::jsonb,
			$7, $8, NULLIF($9, '')::uuid, $10::negocio.resultado_auditoria,
			NULLIF($11, '')::inet, NULLIF($12, '')
		)`,
		tenant, actor.Tipo, actor.ID,
		actor.Agente, actor.CuentaImpersonada, alcance,
		evento.Accion, evento.RecursoTipo, evento.RecursoID, evento.Resultado,
		actor.IP, actor.Dispositivo,
	)
	if err != nil {
		// El error se envuelve con una frase que dice qué implica, porque quien
		// lo lea en un registro tiene que entender por qué se revirtió una
		// operación que en apariencia era correcta.
		return fmt.Errorf("no se pudo auditar la acción, así que no se ejecuta (RNF-36): %w", err)
	}

	return nil
}
