-- =============================================================================
-- 0012 - Plataforma: sesiones, agentes y preferencias
-- =============================================================================
-- Las cinco tablas de ER-02 que 0003 y 0008 dejaron anotadas por escrito:
-- sesion, preferencia_notificacion, agente, autorizacion_agente y token_agente.
-- La razon de que no entraran entonces sigue siendo valida --"una tabla que
-- nadie consulta se equivoca en silencio hasta que alguien la usa"-- y por eso
-- entran ahora: con RF-12, RF-13, RF-21 y RF-25 ya hay codigo que las lee.
--
-- Ninguna lleva tenant_id ni RLS por tenant, igual que cuenta y
-- token_verificacion. La unidad de aislamiento aqui es la CUENTA, no el
-- negocio: una persona tiene una sola cuenta y una sola lista de sesiones
-- aunque reserve en veinte tenants distintos. QUE puede ver esa persona lo
-- siguen decidiendo las politicas de RLS sobre negocio.* cuando la consulta se
-- ejecuta con su tenant fijado.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Tipos
-- -----------------------------------------------------------------------------

-- ER-02 y ER-03 usan (email, sms, push) para notificar, mientras que
-- canal_token --el de la migracion 0008-- solo admite (email, sms). No es la
-- misma lista y no se puede ampliar aquella: un push no transporta un codigo de
-- un solo uso, asi que anadirlo a canal_token permitiria escribir una fila que
-- ningun canal puede entregar. Son dos enums porque son dos dominios.
CREATE TYPE plataforma.canal_notificacion AS ENUM (
  'email',
  'sms',
  'push'
);

CREATE TYPE plataforma.estado_agente AS ENUM (
  'activo',
  'suspendido'
);


-- -----------------------------------------------------------------------------
-- sesion
-- -----------------------------------------------------------------------------
-- RF-25: listar las sesiones activas, revocar una o revocar todas. Y RF-12, que
-- dice que al entrar se "registra el evento de login (IP, dispositivo,
-- timestamp)": ese registro ES esta fila, no una tabla de eventos aparte.
--
-- Aqui vive el token de REFRESCO, no el de acceso. La distincion es la que hace
-- que revocar signifique algo: el de acceso va firmado y se verifica sin tocar
-- la base --es lo que le permite sostener las ~30.000 lecturas/s de RNF-03--,
-- asi que revocarlo de verdad exigiria una consulta por peticion. El de
-- refresco se canjea contra esta tabla, y ahi si se comprueba revocada_en. La
-- consecuencia hay que aceptarla con los ojos abiertos: revocar una sesion corta
-- el refresco al instante y el acceso al vencer, de modo que la ventana de
-- revocacion es exactamente TTL_ACCESO. Acortar esa ventana se hace bajando ese
-- numero, no anadiendo una lectura a la ruta caliente.

CREATE TABLE plataforma.sesion (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  cuenta_id           uuid NOT NULL REFERENCES plataforma.cuenta (id),

  -- SHA-256 del valor en claro, igual que token_verificacion y por la misma
  -- razon (RNF-09): quien lea la tabla no puede suplantar a nadie. Sin sal ni
  -- derivacion lenta, y aqui eso tambien es correcto: no es una contrasena
  -- elegida por una persona sino 256 bits de crypto/rand, asi que no hay
  -- diccionario que probar y encarecer el hash no compra nada.
  token_refresco_hash text NOT NULL,

  dispositivo         text,
  ip                  inet,

  ultimo_acceso       timestamptz NOT NULL DEFAULT now(),
  expira_en           timestamptz NOT NULL,
  revocada_en         timestamptz,
  creada_en           timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT sesion_vigencia_valida
    CHECK (expira_en > creada_en)
);

-- Unico y total, no parcial. Un hash de refresco repetido significaria dos
-- sesiones canjeables con el mismo secreto, y la de menos no se puede
-- distinguir: el indice tiene que cubrir tambien las revocadas y las vencidas
-- para que un valor no se pueda reutilizar nunca.
CREATE UNIQUE INDEX sesion_refresco_uq
  ON plataforma.sesion (token_refresco_hash);

-- La consulta de RF-25: las sesiones vivas de esta cuenta, la mas reciente
-- primero. Parcial sobre las no revocadas porque una revocada no se lista
-- jamas; se conserva por auditoria, no para mostrarla.
CREATE INDEX sesion_activas_por_cuenta
  ON plataforma.sesion (cuenta_id, ultimo_acceso DESC)
  WHERE revocada_en IS NULL;

COMMENT ON TABLE plataforma.sesion IS
  'Sesion de una CUENTA (RF-25). Un invitado no tiene ninguna: su identificacion '
  'se agota en un token de acceso de vigencia corta, y no hay nada que listar '
  'ni que revocar.';

COMMENT ON COLUMN plataforma.sesion.revocada_en IS
  'Revocar no borra: la fila es evidencia de un acceso que existio y la necesita '
  'la auditoria de RNF-36.';


-- -----------------------------------------------------------------------------
-- preferencia_notificacion
-- -----------------------------------------------------------------------------
-- RF-21. La consulta RF-10 en cada envio (RF-10 A4).
--
-- La clave primaria es el triple completo y no hay columna id: la fila NO tiene
-- identidad propia, es la respuesta a "esta cuenta quiere este tipo por este
-- canal". Con un id sustituto cabrian dos filas contradictorias para el mismo
-- triple y habria que decidir cual gana; asi no cabe ninguna.
--
-- La ausencia de fila significa el valor por defecto, no "deshabilitado". Es lo
-- que permite que una cuenta recien creada reciba su confirmacion de reserva sin
-- que nadie haya guardado preferencias todavia, y que anadir un tipo de
-- notificacion nuevo no exija rellenar una fila por cuenta existente.

CREATE TABLE plataforma.preferencia_notificacion (
  cuenta_id         uuid NOT NULL REFERENCES plataforma.cuenta (id),
  canal             plataforma.canal_notificacion NOT NULL,

  -- text y no enum, a diferencia del canal. Los tipos de notificacion crecen
  -- con el producto --confirmacion, recordatorio, cancelacion, review, lista de
  -- espera-- y cada uno nuevo con un enum seria un ALTER TYPE que no puede
  -- correr dentro de una transaccion con el resto de la migracion. El canal no
  -- crece: son las tres formas fisicas de llegar a alguien.
  tipo_notificacion text NOT NULL,

  habilitado        boolean NOT NULL,
  actualizada_en    timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (cuenta_id, canal, tipo_notificacion),

  CONSTRAINT preferencia_tipo_no_vacio
    CHECK (length(trim(tipo_notificacion)) > 0)
);

COMMENT ON TABLE plataforma.preferencia_notificacion IS
  'RF-21. Sin fila = el valor por defecto del tipo, NO "deshabilitado": una '
  'cuenta nueva recibe su confirmacion sin haber guardado nada.';


-- -----------------------------------------------------------------------------
-- agente
-- -----------------------------------------------------------------------------
-- RNF-07: sin registro previo no hay operacion. Un agente es un programa que
-- actua en nombre de personas, no una persona: por eso no esta en cuenta, no
-- tiene correo ni telefono y no puede iniciar sesion como nadie.

CREATE TABLE plataforma.agente (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  nombre           text NOT NULL,

  -- La credencial larga de un cliente automatico, no una contrasena humana:
  -- SHA-256 basta por el mismo motivo que en sesion.
  credencial_hash  text NOT NULL,

  estado           plataforma.estado_agente NOT NULL DEFAULT 'activo',
  creado_en        timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT agente_nombre_no_vacio
    CHECK (length(trim(nombre)) > 0)
);

CREATE UNIQUE INDEX agente_credencial_uq
  ON plataforma.agente (credencial_hash);


-- -----------------------------------------------------------------------------
-- autorizacion_agente
-- -----------------------------------------------------------------------------
-- Ningun agente actua sobre una cuenta que no lo autorizo explicitamente
-- (RNF-07). Es la tabla que convierte "el agente esta registrado" en "el agente
-- puede hablar por ESTA persona", que son dos permisos distintos y solo el
-- segundo importa al emitir un token de RF-13.

CREATE TABLE plataforma.autorizacion_agente (
  cuenta_id   uuid NOT NULL REFERENCES plataforma.cuenta (id),
  agente_id   uuid NOT NULL REFERENCES plataforma.agente (id),

  otorgada_en timestamptz NOT NULL DEFAULT now(),
  revocada_en timestamptz,

  PRIMARY KEY (cuenta_id, agente_id)
);

-- El agente pregunta "que cuentas me autorizaron"; la cuenta pregunta "que
-- agentes autorice". La PK cubre la segunda; este indice, la primera.
CREATE INDEX autorizacion_por_agente
  ON plataforma.autorizacion_agente (agente_id)
  WHERE revocada_en IS NULL;


-- -----------------------------------------------------------------------------
-- token_agente
-- -----------------------------------------------------------------------------
-- El token de RF-13: emitido para UN agente, sobre UNA cuenta impersonada, con
-- UN alcance y de vigencia corta.
--
-- No lleva columna de hash, y eso es deliberado y distinto de sesion. Lo que
-- viaja es un token firmado que transporta este id; la fila no es el secreto,
-- es el registro del permiso. Asi la verificacion de la firma no toca la base
-- --como el token de acceso-- pero el consumo si, porque usado_en tiene que ser
-- atomico: un token de un solo uso comprobado solo contra la firma serviria
-- tantas veces como se presentara.
--
-- alcance es jsonb y no una tabla de acciones porque RF-23 lo define como una
-- INTERSECCION calculada en el momento de emitir --accion solicitada ∩ alcance
-- de la cuenta impersonada--, no como un catalogo administrable. Guardarlo
-- normalizado obligaria a escribir varias filas por token para un valor que
-- nace, se lee entero y muere en minutos.

CREATE TABLE plataforma.token_agente (
  id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  agente_id             uuid NOT NULL REFERENCES plataforma.agente (id),
  cuenta_impersonada_id uuid NOT NULL REFERENCES plataforma.cuenta (id),

  alcance               jsonb NOT NULL,

  expira_en             timestamptz NOT NULL,
  usado_en              timestamptz,
  creado_en             timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT token_agente_vigencia_valida
    CHECK (expira_en > creado_en),

  -- Un alcance vacio es un token que no autoriza nada. Emitirlo no es un caso
  -- degenerado inofensivo: es un token valido que pasa la comprobacion de firma
  -- y despues falla en cada accion, lo que convierte un error de emision en un
  -- error de uso, mucho mas lejos de su causa.
  CONSTRAINT token_agente_alcance_no_vacio
    CHECK (jsonb_typeof(alcance) = 'array' AND jsonb_array_length(alcance) > 0)
);

CREATE INDEX token_agente_vigentes
  ON plataforma.token_agente (agente_id, creado_en DESC)
  WHERE usado_en IS NULL;

COMMENT ON TABLE plataforma.token_agente IS
  'Permiso concreto de RF-13. El valor firmado que viaja transporta este id: la '
  'firma se comprueba sin la base, el consumo (usado_en) no puede.';


-- -----------------------------------------------------------------------------
-- Privilegios
-- -----------------------------------------------------------------------------
-- Sin RLS, por lo dicho en la cabecera. Sin DELETE en ninguna, y en cada caso
-- por un motivo concreto y no por costumbre:
--
--   sesion               -> revocada_en la cierra; la fila es evidencia (RNF-36)
--   autorizacion_agente  -> revocada_en, idem: quien autorizo a quien y cuando
--   token_agente         -> usado_en lo quema; es la traza de RF-36
--   agente               -> estado='suspendido'; borrarlo dejaria tokens huerfanos
--
-- La excepcion es preferencia_notificacion: ahi una fila borrada significa
-- "vuelve al valor por defecto", que es un estado distinto de habilitado=false y
-- el unico que permite que cambiar el defecto del producto alcance a quien nunca
-- toco esa preferencia.

GRANT SELECT, INSERT, UPDATE         ON plataforma.sesion                    TO reservas_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON plataforma.preferencia_notificacion  TO reservas_app;
GRANT SELECT, INSERT, UPDATE         ON plataforma.agente                    TO reservas_app;
GRANT SELECT, INSERT, UPDATE         ON plataforma.autorizacion_agente       TO reservas_app;
GRANT SELECT, INSERT, UPDATE         ON plataforma.token_agente              TO reservas_app;

GRANT SELECT ON plataforma.sesion                   TO reservas_lectura;
GRANT SELECT ON plataforma.preferencia_notificacion TO reservas_lectura;
GRANT SELECT ON plataforma.agente                   TO reservas_lectura;
GRANT SELECT ON plataforma.autorizacion_agente      TO reservas_lectura;
GRANT SELECT ON plataforma.token_agente             TO reservas_lectura;

-- El rol de soporte recibio ALL sobre las tablas que existian en la migracion
-- 0007. GRANT no es retroactivo: las de esta migracion se le conceden aqui o no
-- las tiene, y el super_admin de RF-23 se queda sin poder listar una sesion.
GRANT ALL ON plataforma.sesion                   TO reservas_soporte;
GRANT ALL ON plataforma.preferencia_notificacion TO reservas_soporte;
GRANT ALL ON plataforma.agente                   TO reservas_soporte;
GRANT ALL ON plataforma.autorizacion_agente      TO reservas_soporte;
GRANT ALL ON plataforma.token_agente             TO reservas_soporte;

-- -----------------------------------------------------------------------------
-- Dos correcciones sobre lo que ya existia
-- -----------------------------------------------------------------------------

-- (1) La anonimizacion de RF-25 no cabia en el esquema.
--
-- La migracion 0003 dice, en un comentario: "La anonimizacion pone el email en
-- NULL y el indice parcial deja el camino libre". Pero la misma tabla lleva
-- CHECK (email IS NOT NULL OR telefono IS NOT NULL), asi que ese UPDATE no se
-- puede ejecutar: el CHECK lo rechaza y la cuenta se queda con el correo
-- puesto. Es decir, la tabla documentaba un borrado que ella misma impedia, y
-- nadie lo noto porque hasta ahora nada intentaba eliminar una cuenta.
--
-- La regla correcta es la misma con una excepcion: hace falta un canal de
-- contacto para poder verificar y recuperar la cuenta (RF-19, RF-24), y una
-- cuenta eliminada no se verifica ni se recupera.
ALTER TABLE plataforma.cuenta
  DROP CONSTRAINT cuenta_contacto_minimo;

ALTER TABLE plataforma.cuenta
  ADD CONSTRAINT cuenta_contacto_minimo CHECK (
    estado = 'eliminada'
    OR email IS NOT NULL
    OR telefono IS NOT NULL
  );

COMMENT ON CONSTRAINT cuenta_contacto_minimo ON plataforma.cuenta IS
  'Un canal de contacto es obligatorio salvo en una cuenta eliminada: es lo que '
  'permite la anonimizacion de RF-25 sin borrar la fila que los comprobantes de '
  'RF-34 referencian.';

-- (2) El canje de un enlace busca por valor, no por destino.
--
-- Los dos indices de la migracion 0008 entran por destino, que es lo que
-- necesita un CODIGO: quien lo teclea escribe tambien su correo. Un ENLACE no
-- lleva destino --el token viaja solo en la URL-- asi que su canje busca por
-- valor_hash y sin este indice recorre la tabla entera. Es la consulta de
-- RF-12 (magic link), RF-18 (recuperacion) y RF-19 (verificacion).
--
-- Parcial sobre los no usados por el mismo motivo que su hermano: un token
-- gastado no se vuelve a mirar nunca, y son la mayoria en cuanto el sistema
-- lleve un tiempo funcionando.
CREATE INDEX token_verificacion_por_valor
  ON plataforma.token_verificacion (valor_hash)
  WHERE usado_en IS NULL;
