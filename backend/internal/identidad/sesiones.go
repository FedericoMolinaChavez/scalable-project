package identidad

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/cache"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// hashDeReferencia es un Argon2id real sobre una contraseña que nadie usa.
//
// Existe para una sola cosa: que un correo que NO está registrado tarde lo
// mismo en responder que uno que sí. Sin esto, el "no encontrado" vuelve en
// microsegundos y el "contraseña incorrecta" tarda los ~50 ms de Argon2id, y
// esa diferencia es un oráculo de enumeración tan bueno como responder
// mensajes distintos —justo lo que RF-12 A12 quiere evitar—, solo que
// invisible en el código si nadie lo escribe a propósito.
//
// Se calcula una vez al arrancar y no en cada intento: derivarlo cada vez
// costaría los mismos 19 MiB por petición fallida, que es una forma barata de
// agotar la memoria del servicio desde fuera.
var hashDeReferencia = func() string {
	h, err := HashContrasena("contrasena-de-referencia-que-nadie-usa")
	if err != nil {
		// Solo falla si crypto/rand falla, y entonces el proceso no tiene nada
		// que hacer: sin aleatoriedad no puede emitir un token seguro.
		panic("identidad: no se pudo preparar el hash de referencia: " + err.Error())
	}
	return h
}()

// ContextoSesion es lo que RF-12 pide registrar al entrar: desde donde.
//
// Va como parámetro y no se lee aquí del *http.Request porque este paquete no
// ve peticiones HTTP: es un componente de ARQ-01, no un manejador.
type ContextoSesion struct {
	Dispositivo string
	IP          string
}

// ErrRefrescoInvalido cubre el token de refresco desconocido, el revocado y el
// vencido. Uno solo, por lo mismo de siempre.
var ErrRefrescoInvalido = errors.New("la sesión ya no vale; hay que volver a entrar")

// IniciarSesion comprueba las credenciales y abre una sesión (RF-12).
func (s *Servicio) IniciarSesion(
	ctx context.Context, identificador, contrasena string, donde ContextoSesion,
) (api.ParTokens, error) {
	identificador = normalizar(identificador)
	if !pareceCorreo(identificador) {
		return api.ParTokens{}, ErrCredencialesInvalidas
	}

	var (
		cuentaID   string
		tipo       string
		tenantID   *string
		estado     string
		hash       *string
		correo     *string
		verificado bool
	)

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT id::text, tipo::text, tenant_id::text, estado::text,
			       password_hash, email, email_verificado
			FROM plataforma.cuenta
			WHERE lower(email) = $1 AND estado <> 'eliminada'`,
			identificador).Scan(
			&cuentaID, &tipo, &tenantID, &estado, &hash, &correo, &verificado)
	})

	switch {
	case errors.Is(err, datos.ErrNoEncontrado):
		// Se verifica igualmente contra el hash de referencia. El resultado se
		// descarta; lo que importa es que esta rama tarde lo mismo que la otra.
		_, _ = VerificarContrasena(hashDeReferencia, contrasena)
		return api.ParTokens{}, ErrCredencialesInvalidas
	case err != nil:
		return api.ParTokens{}, err
	}

	// El bloqueo se consulta con la cuenta ya identificada y no con el correo
	// escrito, y esa elección tiene consecuencias: contar por correo dejaría
	// que probar direcciones inexistentes llenara claves en Valkey sin límite.
	clave := "login:" + cuentaID
	if fallos, espera, consultado := s.limites.Consumo(ctx, clave); !consultado {
		// Valkey no responde. Falla CERRADO, igual que el límite de envío de
		// códigos y por el mismo motivo: detrás de este contador no hay
		// ninguna otra capa que frene la fuerza bruta contra una contraseña.
		// El coste de negar es que alguien espere; el de permitir es un
		// millón de intentos por minuto contra una cuenta concreta.
		return api.ParTokens{}, ErrCuentaBloqueada
	} else if fallos >= s.op.MaxIntentosLogin {
		return api.ParTokens{}, fmt.Errorf("%w (faltan %s)", ErrCuentaBloqueada, espera.Round(time.Second))
	}

	if hash == nil {
		// Cuenta sin contraseña: se creó por magic link. Responde igual que una
		// contraseña equivocada —distinguirlo diría que esa dirección existe—
		// pero cuenta el intento, porque desde fuera es indistinguible de un
		// ataque.
		_, _ = VerificarContrasena(hashDeReferencia, contrasena)
		s.limites.Permite(ctx, clave, s.op.MaxIntentosLogin, s.op.BloqueoLogin, cache.Denegar)
		return api.ParTokens{}, ErrCredencialesInvalidas
	}

	coincide, err := VerificarContrasena(*hash, contrasena)
	if err != nil {
		// Un hash ilegible es un fallo del sistema, no una credencial
		// equivocada: devolverlo como 401 lo escondería para siempre.
		return api.ParTokens{}, err
	}
	if !coincide {
		s.limites.Permite(ctx, clave, s.op.MaxIntentosLogin, s.op.BloqueoLogin, cache.Denegar)
		return api.ParTokens{}, ErrCredencialesInvalidas
	}

	// El contador se pone a cero al acertar: RF-12 A4 cuenta fallos
	// CONSECUTIVOS, no fallos totales.
	s.limites.Reiniciar(ctx, clave)

	// El estado se comprueba DESPUÉS de la contraseña (RF-12 A8). Antes
	// filtraría: "esta cuenta está pendiente de verificación" respondido a
	// cualquiera que escriba una dirección es una confirmación de que existe.
	if estado != "activa" {
		return api.ParTokens{}, fmt.Errorf("%w: %s", ErrCuentaNoActiva, estado)
	}

	return s.abrirSesion(ctx, cuentaID, tipo, tenantID, correoVerificado(correo, verificado), donde)
}

// correoVerificado devuelve el correo SOLO si está verificado.
//
// Es lo que decide si el token lleva Destino, y de eso cuelga algo concreto: el
// alcance de una cuenta incluye las reservas que esa persona hizo como invitado
// con ESE correo (RF-24). Un correo sin verificar no puede acreditar nada, o
// bastaría con escribir la dirección de otra persona en el perfil para heredar
// sus reservas de invitado.
//
// Cambiar el correo desde el perfil lo deja sin verificar (RF-22), así que la
// sesión siguiente ya no lo lleva. La en curso sí, hasta que su token venza:
// misma ventana que la revocación, y por la misma razón.
func correoVerificado(correo *string, verificado bool) string {
	if !verificado {
		return ""
	}
	return valor(correo)
}

// SolicitarEnlaceEntrada manda el magic link de RF-12.
//
// Responde igual exista o no la cuenta.
func (s *Servicio) SolicitarEnlaceEntrada(ctx context.Context, destino string) error {
	destino = normalizar(destino)
	if !pareceCorreo(destino) {
		return ErrDestinoInvalido
	}

	if v := s.limites.Permite(
		ctx, "enlace:"+destino, s.op.MaxEnviosHora, time.Hour, cache.Denegar,
	); !v.Permitido {
		return ErrDemasiadosEnvios
	}

	var secreto string

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		secreto = ""

		var cuentaID string
		// Solo una cuenta ACTIVA recibe enlace. Una pendiente de verificación
		// entraría por aquí saltándose la verificación de RF-19, que es
		// exactamente el paso que la activa.
		err := tx.QueryRow(ctx, `
			SELECT id::text FROM plataforma.cuenta
			WHERE lower(email) = $1 AND estado = 'activa'`, destino).Scan(&cuentaID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}

		if secreto, err = generarSecreto(); err != nil {
			return err
		}

		return s.escribirToken(ctx, tx, &cuentaID, PropositoMagicLink, destino, secreto)
	})
	if err != nil {
		return err
	}
	if secreto == "" {
		return nil
	}

	return s.enviarEnlace(ctx, destino, PropositoMagicLink, secreto)
}

// CanjearEnlaceEntrada convierte el magic link en una sesión (RF-12).
func (s *Servicio) CanjearEnlaceEntrada(
	ctx context.Context, valorToken string, donde ContextoSesion,
) (api.ParTokens, error) {
	var (
		cuentaID   string
		tipo       string
		tenantID   *string
		correo     *string
		verificado bool
	)

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		id, _, err := s.consumirToken(ctx, tx, PropositoMagicLink, valorToken)
		if err != nil {
			return err
		}
		if id == nil {
			return fmt.Errorf("magic link sin cuenta asociada")
		}
		cuentaID = *id

		var estado string
		if err := tx.QueryRow(ctx, `
			SELECT tipo::text, tenant_id::text, estado::text, email, email_verificado
			FROM plataforma.cuenta WHERE id = $1`,
			cuentaID).Scan(&tipo, &tenantID, &estado, &correo, &verificado); err != nil {
			return err
		}
		if estado != "activa" {
			return fmt.Errorf("%w: %s", ErrCuentaNoActiva, estado)
		}
		return nil
	})
	if err != nil {
		return api.ParTokens{}, err
	}

	return s.abrirSesion(ctx, cuentaID, tipo, tenantID, correoVerificado(correo, verificado), donde)
}

// Refrescar rota el par de tokens (RF-12).
//
// El refresco presentado deja de valer y se entrega otro. Es lo que hace que un
// refresco copiado se note —el primero de los dos en canjearlo deja al otro
// fuera— en vez de quedar utilizable en paralelo hasta que caduque la sesión.
func (s *Servicio) Refrescar(
	ctx context.Context, refresco string, donde ContextoSesion,
) (api.ParTokens, error) {
	nuevo, err := generarSecreto()
	if err != nil {
		return api.ParTokens{}, err
	}

	var (
		sesionID         string
		cuentaID         string
		tipo             string
		tenantID         *string
		correo           *string
		verificado       bool
		refrescoExpiraEn time.Time
	)

	err = s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		// La rotación es UN update condicionado, no un SELECT seguido de un
		// UPDATE. Con dos pasos, dos canjes simultáneos del mismo valor leen
		// los dos la misma fila y los dos se creen ganadores; así solo uno
		// encuentra la fila con ese hash y el otro no toca nada.
		err := tx.QueryRow(ctx, `
			UPDATE plataforma.sesion
			SET token_refresco_hash = $2,
			    ultimo_acceso = now(),
			    expira_en = now() + make_interval(secs => $3),
			    dispositivo = COALESCE(NULLIF($4, ''), dispositivo),
			    ip = COALESCE(NULLIF($5, '')::inet, ip)
			WHERE token_refresco_hash = $1
			  AND revocada_en IS NULL
			  AND expira_en > now()
			RETURNING id::text, cuenta_id::text, expira_en`,
			huella(refresco), huella(nuevo), s.op.TTLRefresco.Seconds(),
			donde.Dispositivo, donde.IP,
		).Scan(&sesionID, &cuentaID, &refrescoExpiraEn)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRefrescoInvalido
		}
		if err != nil {
			return err
		}

		var estado string
		if err := tx.QueryRow(ctx, `
			SELECT tipo::text, tenant_id::text, estado::text, email, email_verificado
			FROM plataforma.cuenta WHERE id = $1`,
			cuentaID).Scan(&tipo, &tenantID, &estado, &correo, &verificado); err != nil {
			return err
		}
		if estado != "activa" {
			// Una cuenta suspendida después de entrar no puede seguir
			// renovando. Es el punto donde una suspensión surte efecto de
			// verdad, y por eso el refresco se comprueba contra la base y el
			// acceso no.
			return ErrRefrescoInvalido
		}
		return nil
	})
	if err != nil {
		return api.ParTokens{}, err
	}

	acceso, expira, err := s.firmante.EmitirAcceso(Acceso{
		Destino: correoVerificado(correo, verificado),
		Cuenta:  cuentaID,
		Tipo:    tipo,
		Tenant:  valor(tenantID),
		Sesion:  sesionID,
	}, time.Now())
	if err != nil {
		return api.ParTokens{}, err
	}

	return api.ParTokens{
		Acceso:           acceso,
		ExpiraEn:         expira,
		Refresco:         nuevo,
		RefrescoExpiraEn: refrescoExpiraEn,
	}, nil
}

// ListarSesiones devuelve las sesiones vivas de la cuenta (RF-25).
func (s *Servicio) ListarSesiones(
	ctx context.Context, cuentaID, sesionActual string,
) (api.ListaSesiones, error) {
	lista := api.ListaSesiones{Datos: []api.Sesion{}}

	err := s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		lista.Datos = lista.Datos[:0]

		filas, err := tx.Query(ctx, `
			SELECT id::text, dispositivo, host(ip), ultimo_acceso, creada_en, expira_en
			FROM plataforma.sesion
			WHERE cuenta_id = $1 AND revocada_en IS NULL AND expira_en > now()
			ORDER BY ultimo_acceso DESC`, cuentaID)
		if err != nil {
			return err
		}
		defer filas.Close()

		for filas.Next() {
			var (
				sesion api.Sesion
				id     string
			)
			if err := filas.Scan(&id, &sesion.Dispositivo, &sesion.Ip,
				&sesion.UltimoAcceso, &sesion.CreadaEn, &sesion.ExpiraEn); err != nil {
				return err
			}

			parsed, err := uuid.Parse(id)
			if err != nil {
				return err
			}
			sesion.Id = parsed
			sesion.Actual = id == sesionActual

			lista.Datos = append(lista.Datos, sesion)
		}
		return filas.Err()
	})
	if err != nil {
		return api.ListaSesiones{}, err
	}

	return lista, nil
}

// RevocarSesion cierra una sesión concreta (RF-25).
//
// Una sesión de otra cuenta sale como datos.ErrNoEncontrado, igual que una
// inexistente: distinguirlas confirmaría qué identificadores son reales.
func (s *Servicio) RevocarSesion(ctx context.Context, cuentaID, sesionID string) error {
	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		etiqueta, err := tx.Exec(ctx, `
			UPDATE plataforma.sesion
			SET revocada_en = now()
			WHERE id = $1 AND cuenta_id = $2 AND revocada_en IS NULL`, sesionID, cuentaID)
		if err != nil {
			return err
		}
		if etiqueta.RowsAffected() == 0 {
			return datos.ErrNoEncontrado
		}
		return nil
	})
}

// RevocarTodasLasSesiones cierra todas, incluida la que lo pide (RF-25).
//
// Incluir la propia es deliberado: se pulsa cuando se sospecha que alguien más
// entró, y dejar viva justo la sesión desde la que se pulsa obligaría a confiar
// en que la comprometida no es esa.
func (s *Servicio) RevocarTodasLasSesiones(ctx context.Context, cuentaID string) error {
	return s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE plataforma.sesion
			SET revocada_en = now()
			WHERE cuenta_id = $1 AND revocada_en IS NULL`, cuentaID)
		return err
	})
}

// abrirSesion crea la fila de sesión y firma el par de tokens.
//
// La fila ES el registro de login que pide RF-12 —IP, dispositivo, instante—:
// no hay una tabla de eventos de acceso aparte, porque sería una copia de esta
// con los mismos campos.
func (s *Servicio) abrirSesion(
	ctx context.Context, cuentaID, tipo string, tenantID *string, correo string, donde ContextoSesion,
) (api.ParTokens, error) {
	refresco, err := generarSecreto()
	if err != nil {
		return api.ParTokens{}, err
	}

	var (
		sesionID         string
		refrescoExpiraEn time.Time
	)

	err = s.bd.SinTenant(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO plataforma.sesion
				(cuenta_id, token_refresco_hash, dispositivo, ip, expira_en)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')::inet,
			        now() + make_interval(secs => $5))
			RETURNING id::text, expira_en`,
			cuentaID, huella(refresco), donde.Dispositivo, donde.IP, s.op.TTLRefresco.Seconds(),
		).Scan(&sesionID, &refrescoExpiraEn)
	})
	if err != nil {
		return api.ParTokens{}, err
	}

	acceso, expira, err := s.firmante.EmitirAcceso(Acceso{
		Destino: correo,
		Cuenta:  cuentaID,
		Tipo:    tipo,
		Tenant:  valor(tenantID),
		Sesion:  sesionID,
	}, time.Now())
	if err != nil {
		return api.ParTokens{}, err
	}

	return api.ParTokens{
		Acceso:           acceso,
		ExpiraEn:         expira,
		Refresco:         refresco,
		RefrescoExpiraEn: refrescoExpiraEn,
	}, nil
}
