-- =============================================================================
-- Invariantes del modelo fisico
-- =============================================================================
-- Cada caso comprueba que el MOTOR impide algo, no que la aplicacion recuerde
-- impedirlo. Se ejecuta con el rol de aplicacion --no como superusuario-- para
-- que RLS y los privilegios cuenten de verdad.
--
--   psql "postgresql://app_dev:dev@localhost:5432/reservas" \
--        -v ON_ERROR_STOP=1 -f db/pruebas/invariantes.sql
--
-- Todo el guion corre dentro de una transaccion que termina en ROLLBACK: no
-- deja rastro y se puede repetir tantas veces como haga falta.
-- =============================================================================

\set ON_ERROR_STOP on
\pset pager off

BEGIN;

-- Las funciones auxiliares devuelven void, asi que cada llamada imprimiria una
-- fila vacia. El resultado util son los NOTICE, que van por stderr y no se ven
-- afectados por esto.
\o /dev/null

SELECT set_config('app.tenant_id',
                  '11111111-1111-1111-1111-111111111111', true);

-- Comprueba que una sentencia falla con el SQLSTATE esperado. Si pasa, o si
-- falla por otra razon, el guion se detiene.
CREATE FUNCTION pg_temp.debe_fallar(p_sql text, p_sqlstate text, p_caso text)
RETURNS void
LANGUAGE plpgsql
AS $$
DECLARE
  v_paso    boolean := false;
  v_estado  text;
BEGIN
  BEGIN
    EXECUTE p_sql;
    v_paso := true;
  EXCEPTION WHEN OTHERS THEN
    v_estado := SQLSTATE;
  END;

  IF v_paso THEN
    RAISE EXCEPTION 'FALLA [%]: se esperaba el error % y la sentencia paso.',
      p_caso, p_sqlstate;
  ELSIF v_estado <> p_sqlstate THEN
    RAISE EXCEPTION 'FALLA [%]: se esperaba % y ocurrio %.',
      p_caso, p_sqlstate, v_estado;
  END IF;

  RAISE NOTICE 'ok   %  (rechazado con %)', rpad(p_caso, 52), p_sqlstate;
END;
$$;

CREATE FUNCTION pg_temp.debe_pasar(p_sql text, p_caso text)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  EXECUTE p_sql;
  RAISE NOTICE 'ok   %  (aceptado)', rpad(p_caso, 52);
END;
$$;


-- =============================================================================
-- 1. La invariante de RNF-10: no sobreventa
-- =============================================================================

-- Base: 10:00 a 11:00, Sala 1.
SELECT pg_temp.debe_pasar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Base', 'base@ejemplo.test',
     tstzrange('2026-09-07 10:00-05', '2026-09-07 11:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, 'reserva inicial 10:00-11:00');

-- El caso que decide si los limites [) estan bien elegidos. Con limites
-- cerrados por ambos lados, esta reserva compartiria el instante 11:00 con la
-- anterior y el motor la rechazaria: dos citas consecutivas perfectamente
-- validas serian imposibles.
SELECT pg_temp.debe_pasar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Contigua', 'contigua@ejemplo.test',
     tstzrange('2026-09-07 11:00-05', '2026-09-07 12:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, 'cita contigua 11:00-12:00 no solapa');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Intruso', 'intruso@ejemplo.test',
     tstzrange('2026-09-07 10:30-05', '2026-09-07 11:30-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, '23P01', 'solapamiento parcial 10:30-11:30');

-- Cancelar libera el cupo sin borrar la fila: RF-28 conserva la historia y el
-- predicado de la restriccion deja de aplicarle.
SELECT pg_temp.debe_pasar($sql$
  UPDATE negocio.reserva SET estado = 'cancelada'
  WHERE contacto_nombre = 'Base'
$sql$, 'cancelar la reserva base');

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Reemplazo', 'reemplazo@ejemplo.test',
     tstzrange('2026-09-07 10:00-05', '2026-09-07 11:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, 'el cupo cancelado vuelve a estar libre');


-- =============================================================================
-- 2. Reglas de forma de la reserva
-- =============================================================================

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Sin TTL', 'sinttl@ejemplo.test',
     tstzrange('2026-09-08 10:00-05', '2026-09-08 11:00-05', '[)'),
     'pendiente', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, '23514', 'bloqueo pendiente sin expira_en');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Cerrado', 'cerrado@ejemplo.test',
     tstzrange('2026-09-08 10:00-05', '2026-09-08 11:00-05', '[]'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, '23514', 'periodo con limite superior cerrado');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     tstzrange('2026-09-08 12:00-05', '2026-09-08 13:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555')
$sql$, '23514', 'invitado sin datos de contacto');


-- =============================================================================
-- 3. Aislamiento entre tenants (RNF-06)
-- =============================================================================

-- La sede 8888 existe, pero pertenece al tenant 9999. La FK es compuesta:
-- busca (tenant_id = 1111, id = 8888) y no encuentra nada.
--
-- Notese que RLS no interviene aqui, y es deliberado: PostgreSQL exime a las
-- comprobaciones de integridad referencial de las politicas de seguridad,
-- justamente para que RLS no pueda dejar pasar una FK invalida. Quien impide
-- la referencia cruzada es que la clave arrastre tenant_id, no la politica.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.recurso (tenant_id, sede_id, nombre)
  VALUES ('11111111-1111-1111-1111-111111111111',
          '88888888-8888-8888-8888-888888888888',
          'Recurso robado')
$sql$, '23503', 'recurso apuntando a la sede de otro tenant');

-- WITH CHECK: escribir con el tenant_id de otro. Sin esta mitad de la politica,
-- la fila se insertaria --invisible para siempre, pero escrita--, que es
-- corrupcion silenciosa y no un fallo.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.sede (tenant_id, nombre, zona_horaria)
  VALUES ('99999999-9999-9999-9999-999999999999',
          'Sede infiltrada', 'America/New_York')
$sql$, '42501', 'escribir una fila con el tenant_id ajeno');

DO $$
DECLARE v_sedes int;
BEGIN
  SELECT count(*) INTO v_sedes FROM negocio.sede;
  IF v_sedes <> 1 THEN
    RAISE EXCEPTION 'FALLA [RLS lectura]: se ven % sedes, se esperaba 1.', v_sedes;
  END IF;
  RAISE NOTICE 'ok   %  (1 de 2 visibles)',
    rpad('SELECT sin WHERE solo devuelve el propio tenant', 52);
END;
$$;

-- El agujero que RLS por si sola dejaria: leer la particion por su nombre.
-- Se cierra con privilegios, no con politicas.
SELECT pg_temp.debe_fallar(
  'SELECT count(*) FROM negocio.reserva_p00',
  '42501', 'lectura directa de una particion');


-- =============================================================================
-- 4. Inmutabilidad y reglas de negocio (RF-15, RF-17)
-- =============================================================================

SELECT pg_temp.debe_fallar($sql$
  UPDATE negocio.politica_version SET rango_cancelacion_horas = 0
  WHERE id = '55555555-5555-5555-5555-555555555555'
$sql$, '42501', 'modificar una version de politica publicada');

SELECT pg_temp.debe_fallar($sql$
  DELETE FROM negocio.politica_version
  WHERE id = '55555555-5555-5555-5555-555555555555'
$sql$, '42501', 'borrar una version de politica publicada');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.voucher (tenant_id, codigo, porcentaje, limite_usos)
  VALUES ('11111111-1111-1111-1111-111111111111', 'CERO', 0, 10)
$sql$, '23514', 'voucher del 0 por ciento');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.voucher (tenant_id, codigo, porcentaje)
  VALUES ('11111111-1111-1111-1111-111111111111', 'ETERNO', 50)
$sql$, '23514', 'voucher sin limite de usos ni caducidad');

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO negocio.voucher (tenant_id, codigo, porcentaje, limite_usos)
  VALUES ('11111111-1111-1111-1111-111111111111', 'BIENVENIDA', 15, 100)
$sql$, 'voucher valido');

-- RF-17 no permite modificar un voucher, pero el contador de usos SI tiene que
-- poder subir. Se resuelve con privilegios por columna.
SELECT pg_temp.debe_pasar($sql$
  UPDATE negocio.voucher SET usos_actuales = usos_actuales + 1
  WHERE codigo = 'BIENVENIDA'
$sql$, 'incrementar el contador de usos del voucher');

SELECT pg_temp.debe_fallar($sql$
  UPDATE negocio.voucher SET porcentaje = 90 WHERE codigo = 'BIENVENIDA'
$sql$, '42501', 'cambiarle el descuento a un voucher en circulacion');


-- =============================================================================
-- 5. Idempotencia de creacion
-- =============================================================================

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id,
     clave_idempotencia)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Agente', 'agente@ejemplo.test',
     tstzrange('2026-09-09 10:00-05', '2026-09-09 11:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555',
     'clave-del-agente-1')
$sql$, 'primera peticion del agente');

-- El reintento del agente sobre OTRO horario. Sin esta unicidad crearia un
-- segundo bloqueo que nadie liberaria hasta el TTL: denegacion de inventario
-- por reintento, sin mala intencion de por medio.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.reserva
    (tenant_id, servicio_id, recurso_id, contacto_nombre, contacto_email,
     periodo, estado, expira_en, precio_cobrado, moneda, politica_version_id,
     clave_idempotencia)
  VALUES
    ('11111111-1111-1111-1111-111111111111',
     '33333333-3333-3333-3333-333333333333',
     '44444444-4444-4444-4444-444444444444',
     'Agente', 'agente@ejemplo.test',
     tstzrange('2026-09-09 15:00-05', '2026-09-09 16:00-05', '[)'),
     'pendiente', now() + interval '10 min', 80000, 'COP',
     '55555555-5555-5555-5555-555555555555',
     'clave-del-agente-1')
$sql$, '23505', 'reintento del agente con la misma clave');


-- =============================================================================
-- 6. Catalogo y disponibilidad
-- =============================================================================

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.sede (tenant_id, nombre, zona_horaria)
  VALUES ('11111111-1111-1111-1111-111111111111',
          'Sede Fantasma', 'America/Bogata')
$sql$, '23514', 'zona horaria inexistente');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.regla_disponibilidad
    (tenant_id, recurso_id, dia_semana, hora_inicio, hora_fin)
  VALUES ('11111111-1111-1111-1111-111111111111',
          '44444444-4444-4444-4444-444444444444', 1, '22:00', '02:00')
$sql$, '23514', 'franja que cruza medianoche en una sola regla');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO negocio.excepcion_calendario (tenant_id, periodo, tipo)
  VALUES ('11111111-1111-1111-1111-111111111111',
          tstzrange('2026-12-25', '2026-12-26'), 'feriado')
$sql$, '23514', 'excepcion sin recurso ni sede');


-- =============================================================================
-- 7. Cuentas y acceso (ER-02: RF-12, RF-13, RF-21, RF-24, RF-25)
-- =============================================================================
-- Estas tablas no llevan RLS por tenant --la unidad de aislamiento aqui es la
-- CUENTA, no el negocio-- asi que lo que se comprueba no es que un tenant no
-- vea otro, sino que el motor sostenga las reglas de las que depende el acceso.

-- El alcance de RF-23, expresado estructuralmente: un administrador administra
-- UN tenant y un usuario no administra ninguno. No es una convencion, es un
-- CHECK, y por eso no hay tabla de roles que mantener sincronizada.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.cuenta (nombre, email, tipo)
  VALUES ('Admin sin negocio', 'admin-sin-tenant@ejemplo.test', 'administrador')
$sql$, '23514', 'administrador sin tenant');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.cuenta (nombre, email, tipo, tenant_id)
  VALUES ('Usuario con negocio', 'usuario-con-tenant@ejemplo.test', 'usuario',
          '11111111-1111-1111-1111-111111111111')
$sql$, '23514', 'usuario atado a un tenant');

-- Una cuenta necesita un canal de contacto: es por donde se verifica (RF-19) y
-- por donde se recupera (RF-18).
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.cuenta (nombre, tipo) VALUES ('Nadie', 'usuario')
$sql$, '23514', 'cuenta sin correo ni telefono');

-- Salvo si esta eliminada. Es exactamente la anonimizacion de RF-25: la fila se
-- conserva --los comprobantes de RF-34 la referencian-- y lo que desaparece es
-- lo que identifica a una persona.
SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.cuenta (id, nombre, tipo, estado, anonimizada_en)
  VALUES ('99999999-9999-9999-9999-999999999901', NULL, 'usuario', 'eliminada', now())
$sql$, 'cuenta eliminada sin ningun contacto');

-- Y anonimizada_en solo tiene sentido en una cuenta eliminada: una activa con
-- fecha de anonimizacion seria una contradiccion que despues nadie sabria leer.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.cuenta (nombre, email, tipo, estado, anonimizada_en)
  VALUES ('Viva y anonima', 'viva@ejemplo.test', 'usuario', 'activa', now())
$sql$, '23514', 'cuenta activa marcada como anonimizada');

-- El correo es unico entre las cuentas VIVAS, y ese matiz es el que permite
-- volver a registrarse despues de una baja.
SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.cuenta (id, nombre, email, tipo, estado)
  VALUES ('99999999-9999-9999-9999-999999999902',
          'Primera', 'repetido@ejemplo.test', 'usuario', 'activa')
$sql$, 'primera cuenta con un correo');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.cuenta (nombre, email, tipo)
  VALUES ('Segunda', 'REPETIDO@ejemplo.test', 'usuario')
$sql$, '23505', 'segunda cuenta con el mismo correo (sin distinguir mayusculas)');

SELECT pg_temp.debe_pasar($sql$
  UPDATE plataforma.cuenta
  SET email = NULL, nombre = NULL, estado = 'eliminada', anonimizada_en = now()
  WHERE id = '99999999-9999-9999-9999-999999999902'
$sql$, 'anonimizar la cuenta libera su correo');

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.cuenta (nombre, email, tipo)
  VALUES ('Vuelve', 'repetido@ejemplo.test', 'usuario')
$sql$, 'registrarse otra vez con el correo de una cuenta dada de baja');

-- -----------------------------------------------------------------------------
-- sesion (RF-25)
-- -----------------------------------------------------------------------------

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.sesion (id, cuenta_id, token_refresco_hash, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999911',
          '99999999-9999-9999-9999-999999999901', 'huella-de-refresco-1',
          now() + interval '30 days')
$sql$, 'abrir una sesion');

-- Dos sesiones con el mismo refresco serian dos sesiones canjeables con el
-- mismo secreto, y la de menos no se podria distinguir.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.sesion (cuenta_id, token_refresco_hash, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999901', 'huella-de-refresco-1',
          now() + interval '30 days')
$sql$, '23505', 'dos sesiones con el mismo token de refresco');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.sesion (cuenta_id, token_refresco_hash, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999901', 'huella-de-refresco-2',
          now() - interval '1 day')
$sql$, '23514', 'sesion que nace caducada');

-- Revocar no borra: la fila es evidencia de un acceso que existio (RNF-36).
SELECT pg_temp.debe_fallar($sql$
  DELETE FROM plataforma.sesion WHERE id = '99999999-9999-9999-9999-999999999911'
$sql$, '42501', 'borrar una sesion en vez de revocarla');

-- -----------------------------------------------------------------------------
-- preferencia_notificacion (RF-21)
-- -----------------------------------------------------------------------------

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.preferencia_notificacion
    (cuenta_id, canal, tipo_notificacion, habilitado)
  VALUES ('99999999-9999-9999-9999-999999999901', 'email', 'recordatorio', false)
$sql$, 'guardar una preferencia de notificacion');

-- El triple es la clave: no caben dos respuestas contradictorias a "esta cuenta
-- quiere este tipo por este canal".
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.preferencia_notificacion
    (cuenta_id, canal, tipo_notificacion, habilitado)
  VALUES ('99999999-9999-9999-9999-999999999901', 'email', 'recordatorio', true)
$sql$, '23505', 'dos preferencias para el mismo canal y tipo');

-- -----------------------------------------------------------------------------
-- agente y token_agente (RF-13, RNF-07)
-- -----------------------------------------------------------------------------

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.agente (id, nombre, credencial_hash)
  VALUES ('99999999-9999-9999-9999-999999999921', 'Agente de prueba', 'huella-credencial-1')
$sql$, 'registrar un agente');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.agente (nombre, credencial_hash)
  VALUES ('Otro agente', 'huella-credencial-1')
$sql$, '23505', 'dos agentes con la misma credencial');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.agente (nombre, credencial_hash)
  VALUES ('   ', 'huella-credencial-2')
$sql$, '23514', 'agente sin nombre');

-- Un token sin alcance no autoriza nada: pasaria la comprobacion de firma y
-- fallaria en cada accion, convirtiendo un error de emision en uno de uso.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.token_agente
    (agente_id, cuenta_impersonada_id, alcance, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999921',
          '99999999-9999-9999-9999-999999999901',
          '[]'::jsonb, now() + interval '15 min')
$sql$, '23514', 'token de agente con alcance vacio');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.token_agente
    (agente_id, cuenta_impersonada_id, alcance, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999921',
          '99999999-9999-9999-9999-999999999901',
          '{"reservar": true}'::jsonb, now() + interval '15 min')
$sql$, '23514', 'alcance de agente que no es una lista');

SELECT pg_temp.debe_pasar($sql$
  INSERT INTO plataforma.token_agente
    (agente_id, cuenta_impersonada_id, alcance, expira_en)
  VALUES ('99999999-9999-9999-9999-999999999921',
          '99999999-9999-9999-9999-999999999901',
          '["listar_reservas"]'::jsonb, now() + interval '15 min')
$sql$, 'token de agente con un alcance concreto');

-- Un agente no puede hablar por una cuenta que no lo autorizo (RNF-07). Eso lo
-- decide la aplicacion al emitir; lo que el motor sostiene es que la
-- autorizacion apunte a filas que existen.
SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.autorizacion_agente (cuenta_id, agente_id)
  VALUES ('99999999-9999-9999-9999-999999999901',
          '99999999-9999-9999-9999-9999999999ff')
$sql$, '23503', 'autorizacion hacia un agente que no existe');

-- -----------------------------------------------------------------------------
-- token_verificacion (RF-12, RF-18, RF-19)
-- -----------------------------------------------------------------------------

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.token_verificacion
    (proposito, canal, destino, valor_hash, expira_en)
  VALUES ('magic_link', 'email', '  ', 'huella', now() + interval '15 min')
$sql$, '23514', 'token de un solo uso sin destino');

SELECT pg_temp.debe_fallar($sql$
  INSERT INTO plataforma.token_verificacion
    (proposito, canal, destino, valor_hash, expira_en)
  VALUES ('magic_link', 'email', 'x@ejemplo.test', 'huella', now() - interval '1 min')
$sql$, '23514', 'token que nace caducado');


ROLLBACK;

\o
\echo ''
\echo 'Todas las invariantes se cumplen.'
