package identidad

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/correo"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/dominio"
)

// Los propósitos de plataforma.proposito_token que usan las cuentas.
const (
	PropositoVerificacion = "verificacion_contacto" // RF-19
	PropositoMagicLink    = "magic_link"            // RF-12
	PropositoRecuperacion = "recuperacion_password" // RF-18
)

var (
	// ErrCredencialesInvalidas cubre el correo desconocido, la contraseña
	// equivocada y la cuenta que nunca definió contraseña. Es uno solo por la
	// misma razón que ErrCodigoInvalido: cada distinción le dice a quien está
	// probando direcciones cuáles están registradas.
	ErrCredencialesInvalidas = errors.New("el correo o la contraseña no son correctos")

	// ErrCuentaNoActiva sí se distingue, y no contradice lo anterior: quien
	// llega hasta aquí YA acertó la contraseña, así que no se le está diciendo
	// nada que no supiera. RF-12 A8 lo pide explícitamente, porque la acción
	// que corresponde —verificar el correo, o contactar con soporte— depende
	// de cuál de los dos estados sea.
	ErrCuentaNoActiva = errors.New("la cuenta no está activa")

	// ErrCuentaBloqueada es el corte por intentos fallidos consecutivos
	// (RF-12 A3/A4). Es un 429 y no un 401: las credenciales pueden estar
	// perfectamente bien, lo que pasa es que hay que esperar.
	ErrCuentaBloqueada = errors.New("demasiados intentos fallidos seguidos; espera antes de volver a probar")

	// ErrContactoEnUso es el correo o el teléfono que ya tiene otra cuenta
	// (RF-22). Aquí SÍ se dice, al revés que en el alta, y la diferencia es
	// quién pregunta: en el alta pregunta cualquiera, aquí pregunta alguien ya
	// identificado que necesita saber por qué no se guardó su cambio.
	ErrContactoEnUso = errors.New("ese correo o teléfono ya está en uso")

	// ErrTerminosNoAceptados: RF-24 los exige explícitamente.
	ErrTerminosNoAceptados = errors.New("hay que aceptar los términos y condiciones")

	// ErrSinContacto: una cuenta necesita al menos un canal, porque es el
	// único por el que se puede verificar (RF-19) y recuperar (RF-18).
	ErrSinContacto = errors.New("hace falta un correo o un teléfono")

	// ErrCanalNoSoportado: el modelo admite SMS y el producto todavía no tiene
	// proveedor. Se dice en vez de aceptar y no enviar nada, que dejaría a
	// alguien esperando un mensaje que no existe.
	ErrCanalNoSoportado = errors.New("por ahora solo se puede verificar el correo")
)

// tipoNotificacion son los avisos que RF-21 permite configurar. Se enumeran
// aquí y no en el contrato porque la lista la consume RF-10 al enviar, no el
// cliente al pintar: la pantalla muestra lo que el servidor le diga.
var tiposNotificacion = map[string]bool{
	"confirmacion": true,
	"recordatorio": true,
	"cancelacion":  true,
	"review":       true,
	"lista_espera": true,
}

// Registrar da de alta una cuenta (RF-24).
//
// Responde igual exista o no ya ese correo, y esa es la propiedad que gobierna
// toda la función. Si el correo está libre se crea la cuenta en
// `pendiente_verificacion` y sale la verificación de RF-19; si está tomado no
// se crea nada y se avisa al titular de que alguien intentó registrarse con su
// dirección. Desde fuera las dos cosas son un 202 idéntico.
//
// Devuelve error solo cuando el fallo es del sistema o cuando la petición no
// cumple una regla que no depende de si la cuenta existe: términos sin
// aceptar, contraseña que no pasa la política, contacto ausente o mal formado.
func (s *Servicio) Registrar(ctx context.Context, nueva api.NuevaCuenta) error {
	if !nueva.AceptaTerminos {
		return ErrTerminosNoAceptados
	}

	nombre := strings.TrimSpace(nueva.Nombre)
	if nombre == "" {
		return fmt.Errorf("%w: el nombre no puede ir vacío", ErrDestinoInvalido)
	}

	var email, telefono string
	if nueva.Email != nil {
		email = normalizar(string(*nueva.Email))
	}
	if nueva.Telefono != nil {
		telefono = strings.TrimSpace(*nueva.Telefono)
	}
	if email == "" && telefono == "" {
		return ErrSinContacto
	}
	if email == "" {
		// El único canal implementado es el correo: sin él no habría forma de
		// verificar la cuenta y quedaría pendiente para siempre.
		return ErrCanalNoSoportado
	}
	if !pareceCorreo(email) {
		return ErrDestinoInvalido
	}

	// La política de RF-24, con los datos de la propia cuenta como
	// identificadores prohibidos: es lo primero que prueba quien ataca una
	// cuenta concreta.
	if err := dominio.ValidarContrasena(nueva.Contrasena, email, nombre); err != nil {
		return err
	}

	// La cuota va ANTES de escribir nada, igual que en Solicitar y por el mismo
	// motivo: un intento que rebota no debe costar una escritura en el
	// primario. Falla CERRADO porque detrás de este límite tampoco hay ninguna
	// otra capa que impida mandar correo.
	if v := s.limites.Permite(
		ctx, "registro:"+email, s.op.MaxEnviosHora, time.Hour, cache.Denegar,
	); !v.Permitido {
		return ErrDemasiadosEnvios
	}

	hash, err := HashContrasena(nueva.Contrasena)
	if err != nil {
		return err
	}

	var (
		secreto  string
		yaExiste bool
	)

	err = s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		secreto, yaExiste = "", false

		var cuentaID string
		err := tx.QueryRow(ctx, `
			INSERT INTO plataforma.cuenta (nombre, email, telefono, password_hash, tipo, estado)
			VALUES ($1, $2, NULLIF($3, ''), $4, 'usuario', 'pendiente_verificacion')
			ON CONFLICT DO NOTHING
			RETURNING id::text`,
			nombre, email, telefono, hash).Scan(&cuentaID)

		// ON CONFLICT DO NOTHING y no una consulta previa. Comprobar antes e
		// insertar después es una carrera con ventana: dos altas simultáneas
		// con el mismo correo pasan las dos la comprobación. Aquí el índice
		// único arbitra, que es la misma disciplina que el núcleo usa con la
		// restricción EXCLUDE.
		if errors.Is(err, pgx.ErrNoRows) {
			yaExiste = true
			return nil
		}
		if err != nil {
			return err
		}

		secreto, err = generarSecreto()
		if err != nil {
			return err
		}

		return s.escribirToken(ctx, tx, &cuentaID, PropositoVerificacion, email, secreto)
	})
	if err != nil {
		return err
	}

	// El correo va FUERA de la transacción, igual que el código de RF-02: si se
	// enviara dentro y la transacción se revirtiera, existiría en el buzón de
	// alguien un enlace que la base no reconoce.
	if yaExiste {
		return s.avisarIntentoDeAlta(ctx, email)
	}

	return s.enviarEnlace(ctx, email, PropositoVerificacion, secreto)
}

// SolicitarVerificacion reenvía la verificación del contacto (RF-19).
func (s *Servicio) SolicitarVerificacion(ctx context.Context, cuentaID, canal string) error {
	if canal != "email" {
		return ErrCanalNoSoportado
	}

	var (
		email    *string
		yaEstaba bool
		secreto  string
	)

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		email, yaEstaba, secreto = nil, false, ""

		var verificado bool
		if err := tx.QueryRow(ctx, `
			SELECT email, email_verificado
			FROM plataforma.cuenta
			WHERE id = $1 AND estado <> 'eliminada'`, cuentaID).Scan(&email, &verificado); err != nil {
			return err
		}
		if email == nil {
			return ErrSinContacto
		}
		if verificado {
			// No es un error: pedir la verificación de algo ya verificado es
			// una petición redundante, no fallida. No se manda nada.
			yaEstaba = true
			return nil
		}

		var err error
		if secreto, err = generarSecreto(); err != nil {
			return err
		}

		return s.escribirToken(ctx, tx, &cuentaID, PropositoVerificacion, *email, secreto)
	})
	if err != nil {
		return err
	}
	if yaEstaba {
		return nil
	}

	if v := s.limites.Permite(
		ctx, "verificacion:"+*email, s.op.MaxEnviosHora, time.Hour, cache.Denegar,
	); !v.Permitido {
		return ErrDemasiadosEnvios
	}

	return s.enviarEnlace(ctx, *email, PropositoVerificacion, secreto)
}

// CanjearVerificacion marca el contacto como verificado y activa la cuenta
// (RF-19, y el paso final de RF-24).
func (s *Servicio) CanjearVerificacion(ctx context.Context, valor string) (api.Cuenta, error) {
	var cuenta api.Cuenta

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		cuentaID, _, err := s.consumirToken(ctx, tx, PropositoVerificacion, valor)
		if err != nil {
			return err
		}
		if cuentaID == nil {
			// Un token de verificación sin cuenta no lo emite nadie. Si
			// aparece, es un fallo real y no una credencial equivocada.
			return fmt.Errorf("token de verificación sin cuenta asociada")
		}

		// La activación va condicionada al estado: una cuenta suspendida que
		// verifica su correo NO vuelve a estar activa, porque la suspensión no
		// la puso el correo sin verificar.
		if _, err := tx.Exec(ctx, `
			UPDATE plataforma.cuenta
			SET email_verificado = true,
			    estado = CASE WHEN estado = 'pendiente_verificacion' THEN 'activa' ELSE estado END
			WHERE id = $1`, *cuentaID); err != nil {
			return err
		}

		cuenta, err = leerCuenta(ctx, tx, *cuentaID)
		return err
	})
	if err != nil {
		return api.Cuenta{}, err
	}

	return cuenta, nil
}

// Perfil devuelve los datos de la cuenta (RF-22).
func (s *Servicio) Perfil(ctx context.Context, cuentaID string) (api.Cuenta, error) {
	var cuenta api.Cuenta

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		var err error
		cuenta, err = leerCuenta(ctx, tx, cuentaID)
		return err
	})
	if err != nil {
		return api.Cuenta{}, err
	}

	return cuenta, nil
}

// ActualizarPerfil guarda los cambios del perfil (RF-22).
//
// Cambiar el correo lo marca como NO verificado y dispara la verificación. Si
// bastara con escribirlo, apuntar la cuenta a una dirección propia sería
// suficiente para recibir los avisos —y la recuperación de contraseña— de otra
// persona.
func (s *Servicio) ActualizarPerfil(
	ctx context.Context, cuentaID string, cambio api.ActualizacionCuenta,
) (api.Cuenta, error) {
	var nombre, email, telefono *string

	if cambio.Nombre != nil {
		limpio := strings.TrimSpace(*cambio.Nombre)
		if limpio == "" {
			return api.Cuenta{}, fmt.Errorf("%w: el nombre no puede ir vacío", ErrDestinoInvalido)
		}
		nombre = &limpio
	}
	if cambio.Email != nil {
		limpio := normalizar(string(*cambio.Email))
		if !pareceCorreo(limpio) {
			return api.Cuenta{}, ErrDestinoInvalido
		}
		email = &limpio
	}
	if cambio.Telefono != nil {
		limpio := strings.TrimSpace(*cambio.Telefono)
		telefono = &limpio
	}

	var (
		cuenta          api.Cuenta
		correoNuevo     string
		secreto         string
		hayQueVerificar bool
	)

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		correoNuevo, secreto, hayQueVerificar = "", "", false

		// COALESCE deja fuera lo que no viaja: un PATCH que solo trae el nombre
		// no puede borrar el teléfono. El correo, además, se compara con el que
		// hay para saber si CAMBIÓ, porque reenviarse la verificación a la
		// misma dirección desverificaría una cuenta que ya estaba bien.
		var cambioEmail bool
		// Los parámetros van con cast explícito. Sin él, $3 y $4 solo aparecen
		// dentro de COALESCE y de comparaciones con columnas de tipo text, y
		// PostgreSQL no puede inferir su tipo: falla con 42P08 en tiempo de
		// ejecución, no al escribirlo.
		err := tx.QueryRow(ctx, `
			UPDATE plataforma.cuenta
			SET nombre   = COALESCE($2::text, nombre),
			    telefono = COALESCE($3::text, telefono),
			    email    = COALESCE($4::text, email),
			    email_verificado = CASE
			      WHEN $4::text IS NOT NULL
			       AND lower($4::text) IS DISTINCT FROM lower(email)
			      THEN false ELSE email_verificado END,
			    telefono_verificado = CASE
			      WHEN $3::text IS NOT NULL AND $3::text IS DISTINCT FROM telefono
			      THEN false ELSE telefono_verificado END
			WHERE id = $1 AND estado <> 'eliminada'
			RETURNING ($4::text IS NOT NULL AND NOT email_verificado), email`,
			cuentaID, nombre, telefono, email).Scan(&cambioEmail, &correoNuevo)
		if err != nil {
			return err
		}

		if cambioEmail {
			hayQueVerificar = true
			if secreto, err = generarSecreto(); err != nil {
				return err
			}
			if err := s.escribirToken(
				ctx, tx, &cuentaID, PropositoVerificacion, correoNuevo, secreto); err != nil {
				return err
			}
		}

		cuenta, err = leerCuenta(ctx, tx, cuentaID)
		return err
	})
	if errors.Is(err, datos.ErrDuplicado) {
		return api.Cuenta{}, ErrContactoEnUso
	}
	if err != nil {
		return api.Cuenta{}, err
	}

	if hayQueVerificar {
		// Un fallo al enviar NO revierte el cambio: el correo ya está guardado
		// y sin verificar, que es un estado coherente del que se sale pidiendo
		// otro envío. Revertir aquí obligaría a que un relé SMTP caído
		// impidiera editar el perfil.
		if err := s.enviarEnlace(ctx, correoNuevo, PropositoVerificacion, secreto); err != nil {
			s.registro.ErrorContext(ctx, "el perfil se guardó pero no salió la verificación",
				slog.String("error", err.Error()))
		}
	}

	return cuenta, nil
}

// Eliminar anonimiza la cuenta y cierra todo lo suyo (RF-25).
//
// No borra la fila: los comprobantes de RF-34 la referencian y hay obligaciones
// fiscales detrás. Lo que desaparece es lo que identifica a una persona. El
// correo queda libre para volver a registrarse porque los índices de unicidad
// de plataforma.cuenta excluyen las cuentas eliminadas.
func (s *Servicio) Eliminar(ctx context.Context, cuentaID string) error {
	// Primero las reservas futuras, y en transacciones por tenant: negocio.*
	// está bajo RLS y solo se puede tocar con el tenant fijado. La lista de
	// tenants sale de indice_reserva_global, que existe justamente para no
	// tener que abanicar las 64 particiones (RNF-02).
	tenants, err := s.tenantsDeLaCuenta(ctx, cuentaID)
	if err != nil {
		return err
	}

	for _, tenant := range tenants {
		if err := s.cancelarFuturas(ctx, tenant, cuentaID); err != nil {
			return err
		}
	}

	// La anonimización y el cierre de sesiones van juntos en una transacción:
	// una cuenta sin datos pero con sesiones vivas seguiría sirviendo
	// peticiones a nombre de nadie.
	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE plataforma.cuenta
			SET nombre = NULL, email = NULL, telefono = NULL, password_hash = NULL,
			    email_verificado = false, telefono_verificado = false,
			    estado = 'eliminada', anonimizada_en = now()
			WHERE id = $1`, cuentaID); err != nil {
			return err
		}

		if _, err := tx.Exec(ctx, `
			UPDATE plataforma.sesion
			SET revocada_en = now()
			WHERE cuenta_id = $1 AND revocada_en IS NULL`, cuentaID); err != nil {
			return err
		}

		// Las preferencias sí se borran, y es la única tabla de la que se
		// borra: no son evidencia de nada y son datos de una persona que pidió
		// que no quedaran.
		_, err := tx.Exec(ctx,
			"DELETE FROM plataforma.preferencia_notificacion WHERE cuenta_id = $1", cuentaID)
		return err
	})
}

func (s *Servicio) tenantsDeLaCuenta(ctx context.Context, cuentaID string) ([]string, error) {
	var tenants []string

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		tenants = nil

		filas, err := tx.Query(ctx, `
			SELECT DISTINCT tenant_id::text
			FROM plataforma.indice_reserva_global
			WHERE cuenta_id = $1`, cuentaID)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var tenant string
			if err := filas.Scan(&tenant); err != nil {
				return err
			}
			tenants = append(tenants, tenant)
		}
		return filas.Err()
	})

	return tenants, err
}

// cancelarFuturas cancela lo que todavía no ocurrió, dentro de un tenant.
//
// Solo lo futuro. Una reserva pasada es historia y cancelarla mentiría sobre lo
// que ocurrió; una en curso tampoco se toca, porque alguien está siendo
// atendido ahora mismo.
func (s *Servicio) cancelarFuturas(ctx context.Context, tenant, cuentaID string) error {
	return s.bd.EnTenant(ctx, tenant, func(tx pgx.Tx) error {
		filas, err := tx.Query(ctx, `
			UPDATE negocio.reserva
			SET estado = 'cancelada'
			WHERE cuenta_id = $1
			  AND estado IN ('pendiente', 'confirmada')
			  AND lower(periodo) > now()
			RETURNING id::text, estado::text`, cuentaID)
		if err != nil {
			return err
		}

		var ids []string
		for filas.Next() {
			var id, estado string
			if err := filas.Scan(&id, &estado); err != nil {
				filas.Close()
				return err
			}
			ids = append(ids, id)
		}
		filas.Close()
		if err := filas.Err(); err != nil {
			return err
		}

		// La transición se escribe en la MISMA transacción que el cambio
		// (RF-28). El actor es 'usuario' con su cuenta: fue una persona quien
		// pidió la baja, no un barrido del sistema, y transicion_actor_coherente
		// exige que solo el sistema tenga actor_id nulo.
		for _, id := range ids {
			if _, err := tx.Exec(ctx, `
				INSERT INTO negocio.transicion_estado
					(tenant_id, reserva_id, estado_anterior, estado_nuevo, actor_tipo, actor_id, motivo)
				VALUES ($1, $2, 'confirmada', 'cancelada', 'usuario', $3, $4)`,
				tenant, id, cuentaID,
				"la cuenta se eliminó (RF-25)"); err != nil {
				return err
			}
		}

		return nil
	})
}

// SolicitarRecuperacion manda el enlace de RF-18.
//
// Responde igual exista o no la cuenta y no envía nada cuando no existe. Es el
// mismo criterio de RF-12 A12.
func (s *Servicio) SolicitarRecuperacion(ctx context.Context, destino string) error {
	destino = normalizar(destino)
	if !pareceCorreo(destino) {
		return ErrDestinoInvalido
	}

	if v := s.limites.Permite(
		ctx, "recuperacion:"+destino, s.op.MaxEnviosHora, time.Hour, cache.Denegar,
	); !v.Permitido {
		return ErrDemasiadosEnvios
	}

	var secreto string

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		secreto = ""

		var cuentaID string
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM plataforma.cuenta
			WHERE lower(email) = $1 AND estado NOT IN ('eliminada')`, destino).Scan(&cuentaID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		if secreto, err = generarSecreto(); err != nil {
			return err
		}

		return s.escribirToken(ctx, tx, &cuentaID, PropositoRecuperacion, destino, secreto)
	})
	if err != nil {
		return err
	}
	if secreto == "" {
		return nil
	}

	return s.enviarEnlace(ctx, destino, PropositoRecuperacion, secreto)
}

// Restablecer cambia la contraseña con el enlace de recuperación (RF-18).
//
// Revoca TODAS las sesiones. Una contraseña se restablece justo cuando se
// sospecha que alguien más la tiene, y dejar vivas las sesiones abiertas con la
// anterior vaciaría el gesto.
func (s *Servicio) Restablecer(ctx context.Context, valor, contrasenaNueva string) error {
	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		cuentaID, _, err := s.consumirToken(ctx, tx, PropositoRecuperacion, valor)
		if err != nil {
			return err
		}
		if cuentaID == nil {
			return fmt.Errorf("token de recuperación sin cuenta asociada")
		}

		// La política se comprueba DESPUÉS de consumir el token, y el orden es
		// deliberado: si se comprobara antes, una contraseña rechazada dejaría
		// el enlace vivo y quien lo tuviera podría seguir probando.
		correo, nombre, err := contactoDe(ctx, tx, *cuentaID)
		if err != nil {
			return err
		}
		if err := dominio.ValidarContrasena(contrasenaNueva, correo, nombre); err != nil {
			return err
		}

		return s.fijarContrasena(ctx, tx, *cuentaID, contrasenaNueva, "")
	})
}

// CambiarContrasena la cambia con la contraseña actual como prueba (RF-18).
//
// Se exige la actual aunque ya haya sesión: un token robado no debe bastar para
// quedarse con la cuenta. Y se conserva la sesión desde la que se hace, porque
// quien la hace ya demostró saber la anterior.
func (s *Servicio) CambiarContrasena(
	ctx context.Context, cuentaID, sesionID, actual, nueva string,
) error {
	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		var hash *string
		if err := tx.QueryRow(ctx, `
			SELECT password_hash FROM plataforma.cuenta
			WHERE id = $1 AND estado <> 'eliminada'`, cuentaID).Scan(&hash); err != nil {
			return err
		}
		if hash == nil {
			return ErrCredencialesInvalidas
		}

		coincide, err := VerificarContrasena(*hash, actual)
		if err != nil {
			return err
		}
		if !coincide {
			return ErrCredencialesInvalidas
		}

		correo, nombre, err := contactoDe(ctx, tx, cuentaID)
		if err != nil {
			return err
		}
		if err := dominio.ValidarContrasena(nueva, correo, nombre); err != nil {
			return err
		}

		return s.fijarContrasena(ctx, tx, cuentaID, nueva, sesionID)
	})
}

// fijarContrasena escribe el hash nuevo y revoca sesiones.
//
// `conservar` es la sesión que sobrevive, o vacío para revocarlas todas.
func (s *Servicio) fijarContrasena(
	ctx context.Context, tx pgx.Tx, cuentaID, contrasena, conservar string,
) error {
	hash, err := HashContrasena(contrasena)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		"UPDATE plataforma.cuenta SET password_hash = $2 WHERE id = $1", cuentaID, hash); err != nil {
		return err
	}

	// Los tokens de recuperación pendientes se queman junto con la contraseña.
	// Si quedaran vivos, un enlace pedido antes seguiría sirviendo para cambiar
	// la contraseña que se acaba de poner.
	if _, err := tx.Exec(ctx, `
		UPDATE plataforma.token_verificacion
		SET usado_en = now()
		WHERE cuenta_id = $1 AND proposito = $2 AND usado_en IS NULL`,
		cuentaID, PropositoRecuperacion); err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE plataforma.sesion
		SET revocada_en = now()
		WHERE cuenta_id = $1
		  AND revocada_en IS NULL
		  AND ($2 = '' OR id <> $2::uuid)`, cuentaID, conservar)
	return err
}

// Preferencias devuelve lo que la cuenta guardó explícitamente (RF-21).
func (s *Servicio) Preferencias(ctx context.Context, cuentaID string) (api.ListaPreferencias, error) {
	lista := api.ListaPreferencias{Datos: []api.Preferencia{}}

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		lista.Datos = lista.Datos[:0]

		filas, err := tx.Query(ctx, `
			SELECT canal::text, tipo_notificacion, habilitado
			FROM plataforma.preferencia_notificacion
			WHERE cuenta_id = $1
			ORDER BY canal, tipo_notificacion`, cuentaID)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var p api.Preferencia
			var canal string
			if err := filas.Scan(&canal, &p.Tipo, &p.Habilitado); err != nil {
				return err
			}
			p.Canal = api.CanalNotificacion(canal)
			lista.Datos = append(lista.Datos, p)
		}
		return filas.Err()
	})
	if err != nil {
		return api.ListaPreferencias{}, err
	}

	return lista, nil
}

// GuardarPreferencias reemplaza el conjunto completo (RF-21).
//
// Borra y reinserta dentro de una transacción, en vez de hacer un upsert por
// fila. Es lo que hace que quitar una preferencia signifique "vuelve al valor
// por defecto": con upsert, lo que no viaja se queda como estaba y no habría
// forma de expresar esa vuelta.
func (s *Servicio) GuardarPreferencias(
	ctx context.Context, cuentaID string, lista api.ListaPreferencias,
) (api.ListaPreferencias, error) {
	for _, p := range lista.Datos {
		if !tiposNotificacion[p.Tipo] {
			return api.ListaPreferencias{}, fmt.Errorf(
				"%w: tipo de notificación desconocido: %q", ErrDestinoInvalido, p.Tipo)
		}
	}

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"DELETE FROM plataforma.preferencia_notificacion WHERE cuenta_id = $1",
			cuentaID); err != nil {
			return err
		}

		for _, p := range lista.Datos {
			if _, err := tx.Exec(ctx, `
				INSERT INTO plataforma.preferencia_notificacion
					(cuenta_id, canal, tipo_notificacion, habilitado)
				VALUES ($1, $2, $3, $4)`,
				cuentaID, string(p.Canal), p.Tipo, p.Habilitado); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return api.ListaPreferencias{}, err
	}

	return s.Preferencias(ctx, cuentaID)
}

// ------------------------------------------------------------- auxiliares --

// generarSecreto produce el valor de un enlace de un solo uso.
//
// 256 bits de crypto/rand, no seis dígitos. Un enlace no se teclea, así que su
// longitud no cuesta nada, y a cambio no necesita contador de intentos: un
// espacio de 2^256 no se recorre. Es exactamente el motivo por el que su hash
// puede ser SHA-256 a secas.
func generarSecreto() (string, error) {
	crudo := make([]byte, 32)
	if _, err := rand.Read(crudo); err != nil {
		return "", fmt.Errorf("no se pudo generar el secreto: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(crudo), nil
}

// escribirToken guarda la huella de un valor de un solo uso.
//
// Quema antes los del mismo destino y propósito, por lo mismo que Solicitar:
// tres enlaces vivos a la vez son tres puertas abiertas.
func (s *Servicio) escribirToken(
	ctx context.Context, tx pgx.Tx, cuentaID *string, proposito, destino, secreto string,
) error {
	if _, err := tx.Exec(ctx, `
		UPDATE plataforma.token_verificacion
		SET usado_en = now()
		WHERE destino = $1 AND proposito = $2 AND usado_en IS NULL`,
		destino, proposito); err != nil {
		return err
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO plataforma.token_verificacion
			(cuenta_id, proposito, canal, destino, valor_hash, expira_en)
		VALUES ($1, $2, 'email', $3, $4, now() + make_interval(secs => $5))`,
		cuentaID, proposito, destino, huella(secreto), s.op.TTLEnlace.Seconds())
	return err
}

// consumirToken comprueba un valor de un solo uso y lo quema.
//
// Devuelve la cuenta y el destino. Un valor equivocado, caducado, ya usado o
// inexistente sale como ErrCodigoInvalido, sin distinguirlos: cada distinción
// le diría a quien está probando enlaces si va por buen camino.
//
// A diferencia del canje de un código, aquí NO hay contador de intentos y no
// hace falta: el valor tiene 256 bits, así que no se adivina, y el contador
// solo existe para acotar un espacio pequeño.
func (s *Servicio) consumirToken(
	ctx context.Context, tx pgx.Tx, proposito, valor string,
) (*string, string, error) {
	valor = strings.TrimSpace(valor)
	if valor == "" {
		return nil, "", ErrCodigoInvalido
	}

	var (
		id       string
		cuentaID *string
		destino  string
	)

	// La búsqueda es por valor_hash y no por destino: un enlace viaja solo, sin
	// que quien lo abre escriba su correo. FOR UPDATE serializa dos canjes
	// simultáneos del mismo enlace, que si no lo consumirían los dos.
	err := tx.QueryRow(ctx, `
		SELECT id::text, cuenta_id::text, destino
		FROM plataforma.token_verificacion
		WHERE valor_hash = $1
		  AND proposito = $2
		  AND usado_en IS NULL
		  AND expira_en > now()
		FOR UPDATE`, huella(valor), proposito).Scan(&id, &cuentaID, &destino)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", ErrCodigoInvalido
	}
	if err != nil {
		return nil, "", err
	}

	if _, err := tx.Exec(ctx,
		"UPDATE plataforma.token_verificacion SET usado_en = now() WHERE id = $1", id); err != nil {
		return nil, "", err
	}

	return cuentaID, destino, nil
}

func leerCuenta(ctx context.Context, tx pgx.Tx, cuentaID string) (api.Cuenta, error) {
	var (
		cuenta   api.Cuenta
		id       string
		tipo     string
		estado   string
		email    *string
		tenantID *string
	)

	if err := tx.QueryRow(ctx, `
		SELECT id::text, nombre, email, telefono, email_verificado, telefono_verificado,
		       tipo::text, tenant_id::text, estado::text, creada_en
		FROM plataforma.cuenta
		WHERE id = $1`, cuentaID).Scan(
		&id, &cuenta.Nombre, &email, &cuenta.Telefono,
		&cuenta.EmailVerificado, &cuenta.TelefonoVerificado,
		&tipo, &tenantID, &estado, &cuenta.CreadaEn,
	); err != nil {
		return api.Cuenta{}, err
	}

	parsed, err := uuid.Parse(id)
	if err != nil {
		return api.Cuenta{}, err
	}
	cuenta.Id = parsed
	cuenta.Tipo = api.TipoCuenta(tipo)
	cuenta.Estado = api.EstadoCuenta(estado)

	if email != nil {
		correo := openapi_types.Email(*email)
		cuenta.Email = &correo
	}
	if tenantID != nil {
		tenant, err := uuid.Parse(*tenantID)
		if err != nil {
			return api.Cuenta{}, err
		}
		cuenta.TenantId = &tenant
	}

	return cuenta, nil
}

// contactoDe devuelve el correo y el nombre, que son lo que la política de
// contraseñas no debe dejar aparecer dentro de la contraseña.
func contactoDe(ctx context.Context, tx pgx.Tx, cuentaID string) (string, string, error) {
	var email, nombre *string

	if err := tx.QueryRow(ctx,
		"SELECT email, nombre FROM plataforma.cuenta WHERE id = $1",
		cuentaID).Scan(&email, &nombre); err != nil {
		return "", "", err
	}

	return valor(email), valor(nombre), nil
}

func valor(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---------------------------------------------------------------- correos --

// enlacePara construye la URL que viaja en el correo.
//
// Apunta al FRONTEND y no a esta API: quien abre el enlace es una persona con
// un navegador, y una respuesta JSON en pantalla no es una confirmación de
// nada. El backend lo recibe después, cuando esa pantalla canjea el valor.
func (s *Servicio) enlacePara(proposito, secreto string) string {
	ruta := map[string]string{
		PropositoVerificacion: "/verificar",
		PropositoMagicLink:    "/entrar",
		PropositoRecuperacion: "/restablecer",
	}[proposito]

	return strings.TrimRight(s.op.BaseURL, "/") + ruta + "?token=" + secreto
}

func (s *Servicio) enviarEnlace(ctx context.Context, destino, proposito, secreto string) error {
	asunto, cuerpo := textoEnlace(proposito, s.enlacePara(proposito, secreto), s.op.TTLEnlace)

	if err := s.emisor.Enviar(ctx, correo.Mensaje{
		Para: destino, Asunto: asunto, Cuerpo: cuerpo,
	}); err != nil {
		return fmt.Errorf("no se pudo enviar el enlace: %w", err)
	}
	return nil
}

func textoEnlace(proposito, enlace string, vigencia time.Duration) (string, string) {
	minutos := int(vigencia.Minutes())

	switch proposito {
	case PropositoVerificacion:
		return "Confirma tu correo", fmt.Sprintf(
			"Para terminar de crear tu cuenta, abre este enlace:\n\n    %s\n\n"+
				"Caduca en %d minutos y solo sirve una vez.\n\n"+
				"Si no fuiste tú, no hace falta que hagas nada: sin abrirlo, la cuenta\n"+
				"no se activa.\n", enlace, minutos)

	case PropositoMagicLink:
		return "Tu enlace para entrar", fmt.Sprintf(
			"Abre este enlace para entrar sin contraseña:\n\n    %s\n\n"+
				"Caduca en %d minutos y solo sirve una vez.\n\n"+
				"Si no lo pediste tú, ignóralo: sin el enlace nadie puede entrar.\n",
			enlace, minutos)

	default:
		return "Restablece tu contraseña", fmt.Sprintf(
			"Pediste cambiar tu contraseña. Abre este enlace para elegir una nueva:\n\n    %s\n\n"+
				"Caduca en %d minutos y solo sirve una vez.\n\n"+
				"Si no lo pediste tú, ignóralo: tu contraseña actual sigue siendo válida\n"+
				"y nadie ha entrado.\n", enlace, minutos)
	}
}

// avisarIntentoDeAlta es la otra mitad de la anti-enumeración de RF-24.
//
// Quien prueba direcciones recibe siempre el mismo 202, y el titular de la
// dirección se entera de que alguien lo intentó. Sin este correo, el silencio
// protegería igual de la enumeración pero dejaría al titular sin saber nada.
func (s *Servicio) avisarIntentoDeAlta(ctx context.Context, destino string) error {
	if err := s.emisor.Enviar(ctx, correo.Mensaje{
		Para:   destino,
		Asunto: "Alguien intentó crear una cuenta con tu correo",
		Cuerpo: "Ya existe una cuenta con esta dirección, así que no se creó ninguna nueva.\n\n" +
			"Si fuiste tú y no recuerdas la contraseña, pide un enlace de recuperación\n" +
			"desde la pantalla de inicio de sesión.\n\n" +
			"Si no fuiste tú, no hace falta que hagas nada: tu cuenta no ha cambiado.\n",
	}); err != nil {
		return fmt.Errorf("no se pudo avisar del intento de alta: %w", err)
	}
	return nil
}
