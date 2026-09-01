-- =============================================================================
-- 0008 - Plataforma: tokens de verificacion
-- =============================================================================
-- La puerta por la que un invitado vuelve a sus reservas (RF-02).
--
-- De ER-02 entra SOLO token_verificacion. Las otras cinco tablas de esa lamina
-- --sesion, preferencia_notificacion, agente, autorizacion_agente,
-- token_agente-- pertenecen a requisitos que todavia no tienen codigo, y una
-- tabla que nadie consulta es una tabla que nadie mantiene: se equivoca en
-- silencio hasta que alguien la usa. Entran con RF-12, RF-13 y RF-21.
--
-- Por que un invitado no usa sesion: sesion.cuenta_id es NOT NULL, y la
-- gestiona RF-25 --listar y revocar las sesiones DE UNA CUENTA--. Quien reservo
-- como invitado no tiene cuenta ni nada que listar. Su identificacion se agota
-- en un token de acceso de vigencia corta, que es lo que RF-12 llama "token de
-- acceso", separado del de refresco que si vive en sesion.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Tipos
-- -----------------------------------------------------------------------------
-- Los dos enums completos de ER-02, no solo los valores que esta fase usa.
-- Ampliar un enum despues es un ALTER TYPE que no puede correr dentro de una
-- transaccion con otras sentencias, asi que enumerarlos ya sale mas barato que
-- ir anadiendolos de uno en uno.

CREATE TYPE plataforma.proposito_token AS ENUM (
  'verificacion_contacto',   -- RF-19
  'magic_link',              -- RF-12, inicio de sesion sin contrasena
  'codigo_sms',              -- RF-12
  'recuperacion_password',   -- RF-18
  'consulta_reservas'        -- RF-02: el invitado vuelve a ver lo suyo
);

CREATE TYPE plataforma.canal_token AS ENUM (
  'email',
  'sms'
);


-- -----------------------------------------------------------------------------
-- token_verificacion
-- -----------------------------------------------------------------------------

CREATE TABLE plataforma.token_verificacion (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  -- NULL = invitado (RF-02). Es el caso normal en esta fase: quien reserva sin
  -- cuenta solo deja un correo de contacto en la reserva, y ese correo es todo
  -- lo que hay para identificarlo despues.
  cuenta_id   uuid REFERENCES plataforma.cuenta (id),

  proposito   plataforma.proposito_token NOT NULL,
  canal       plataforma.canal_token     NOT NULL,
  destino     text        NOT NULL,
  valor_hash  text        NOT NULL,
  expira_en   timestamptz NOT NULL,
  usado_en    timestamptz,
  intentos    int         NOT NULL DEFAULT 0,
  creado_en   timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT token_destino_no_vacio
    CHECK (length(trim(destino)) > 0),

  CONSTRAINT token_intentos_no_negativos
    CHECK (intentos >= 0),

  CONSTRAINT token_vigencia_valida
    CHECK (expira_en > creado_en)
);

COMMENT ON TABLE plataforma.token_verificacion IS
  'Codigos y enlaces de un solo uso. usado_en los invalida; expira_en los caduca. '
  'Global, sin tenant_id: una persona es la misma aunque reserve en varios negocios.';

COMMENT ON COLUMN plataforma.token_verificacion.valor_hash IS
  'NUNCA el codigo en claro (RNF-09). Quien lea esta tabla no puede suplantar a '
  'nadie: solo puede comprobar un codigo que ya tenga.';

COMMENT ON COLUMN plataforma.token_verificacion.intentos IS
  'Intentos fallidos de canje. RF-12 A2 corta a los 3: sin contador, un codigo '
  'de 6 digitos se adivina por fuerza bruta en un millon de peticiones, que no '
  'es un numero grande para una maquina.';


-- -----------------------------------------------------------------------------
-- Indices
-- -----------------------------------------------------------------------------

-- La consulta del canje: el token vigente de este destino para este proposito.
-- Parcial sobre los no usados porque los gastados no se vuelven a mirar nunca,
-- y son la mayoria en cuanto el sistema lleve un tiempo funcionando.
CREATE INDEX token_verificacion_vigente
  ON plataforma.token_verificacion (destino, proposito, creado_en DESC)
  WHERE usado_en IS NULL;

-- El limite de reenvios de RF-12 A11: cuantos se han pedido para este destino
-- en la ultima hora. Mira tambien los ya usados --pedir tres codigos y gastar
-- los tres sigue siendo pedir tres--, asi que este no puede ser parcial.
CREATE INDEX token_verificacion_por_destino
  ON plataforma.token_verificacion (destino, creado_en DESC);


-- -----------------------------------------------------------------------------
-- Privilegios
-- -----------------------------------------------------------------------------
-- Sin RLS: esta tabla no lleva tenant_id y no puede llevarlo. Una persona es la
-- misma aunque reserve en veinte negocios, igual que cuenta y sesion (ER-02).
--
-- El aislamiento no se pierde por eso: el token solo dice "quien demostro tener
-- este correo". QUE puede ver esa persona lo siguen decidiendo las politicas de
-- RLS sobre negocio.* cuando la consulta se ejecuta con su tenant fijado.

GRANT SELECT, INSERT, UPDATE ON plataforma.token_verificacion TO reservas_app;
GRANT SELECT                  ON plataforma.token_verificacion TO reservas_lectura;

-- Sin DELETE ni para la aplicacion: un token gastado es evidencia de un intento
-- de acceso y lo necesita la auditoria de RNF-36. La purga por antiguedad es
-- trabajo de un proceso de mantenimiento, no de la ruta caliente.


-- -----------------------------------------------------------------------------
-- El quinto indice de reserva
-- -----------------------------------------------------------------------------
-- La migracion 0006 dejo cuatro y dijo "ni uno mas": cada indice adicional es
-- trabajo en cada una de las ~3.300 inserciones por segundo de RNF-03, asi que
-- tiene que justificar una consulta real. Este la tiene, y no la tenia antes
-- porque no habia forma de que un invitado volviera.
--
-- reserva_por_cuenta cubre RF-02 para quien TIENE cuenta, y es explicitamente
-- parcial sobre cuenta_id NOT NULL. La otra mitad de RF-02 --el invitado que se
-- identifica con un OTP al correo con que reservo-- busca justo por el lado que
-- aquel indice excluye. Sin este, esa consulta escanea las 64 particiones.
--
-- Las dos mitades son disjuntas por construccion, asi que entre los dos indices
-- no hay ni una fila repetida.
CREATE INDEX reserva_por_contacto
  ON negocio.reserva (tenant_id, lower(contacto_email), creada_en DESC)
  WHERE cuenta_id IS NULL AND contacto_email IS NOT NULL;

COMMENT ON INDEX negocio.reserva_por_contacto IS
  'RF-02 para invitados. lower() porque los correos se comparan sin distinguir '
  'mayusculas: quien escribe Ana@ejemplo.com al reservar teclea ana@ejemplo.com '
  'al volver, y son la misma persona.';
