-- =============================================================================
-- 0003 - Plataforma: tenant, cuenta e indice global de reservas
-- =============================================================================
-- Solo las tres tablas globales que la ruta de reserva necesita. El resto de
-- ER-02 --sesion, token_verificacion, preferencia_notificacion, agente,
-- autorizacion_agente, token_agente-- entra en la migracion 0008: son la
-- superficie de autenticacion, no la de reserva, y no aparecen en ninguna FK
-- de negocio.
--
-- Ninguna de estas tablas se particiona ni lleva tenant_id como discriminante.
-- Una persona tiene UNA cuenta aunque reserve en veinte negocios distintos.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- tenant
-- -----------------------------------------------------------------------------
-- Es la raiz de todo el particionado y, al mismo tiempo, la unica tabla que no
-- se puede particionar por tenant_id sin caer en una recursion.

CREATE TABLE plataforma.tenant (
  id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  identificador  text        NOT NULL,
  nombre         text        NOT NULL,
  moneda         char(3)     NOT NULL,
  zona_horaria   text        NOT NULL,
  estado         plataforma.estado_tenant NOT NULL DEFAULT 'activo',
  creado_en      timestamptz NOT NULL DEFAULT now(),

  CONSTRAINT tenant_identificador_uq
    UNIQUE (identificador),

  -- El identificador va en subdominios y en URLs: se acota a lo que sobrevive
  -- a un DNS y no se puede confundir visualmente con otro.
  CONSTRAINT tenant_identificador_formato
    CHECK (identificador ~ '^[a-z0-9]([a-z0-9-]{1,30}[a-z0-9])$'),

  CONSTRAINT tenant_moneda_iso
    CHECK (moneda ~ '^[A-Z]{3}$'),

  CONSTRAINT tenant_zona_horaria_valida
    CHECK (infra.zona_horaria_valida(zona_horaria))
);

COMMENT ON TABLE plataforma.tenant IS
  'Negocio. Unica fuente de la moneda: ninguna otra tabla la define, solo la copia.';
COMMENT ON COLUMN plataforma.tenant.moneda IS
  'ISO 4217. reserva y pago guardan una copia como parte del snapshot financiero (RF-34).';


-- -----------------------------------------------------------------------------
-- cuenta
-- -----------------------------------------------------------------------------
-- Los tres tipos de RF-23. El alcance de cada uno no se guarda en una tabla de
-- roles: es una consecuencia estructural de dos columnas, y el CHECK lo vuelve
-- imposible de violar.
--
--   usuario       -> tenant_id NULL      (reserva donde quiera)
--   administrador -> tenant_id NOT NULL  (dueno de ese negocio)
--   super_admin   -> tenant_id NULL      (alcance sobre todos)
--
-- No hay asignacion de roles porque no hay roles que asignar. Eso fue una
-- correccion explicita al diseno de RF-23.

CREATE TABLE plataforma.cuenta (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),

  nombre              text,
  email               text,
  telefono            text,
  email_verificado    boolean NOT NULL DEFAULT false,
  telefono_verificado boolean NOT NULL DEFAULT false,
  password_hash       text,

  tipo                plataforma.tipo_cuenta   NOT NULL,
  tenant_id           uuid REFERENCES plataforma.tenant (id),
  estado              plataforma.estado_cuenta NOT NULL
                        DEFAULT 'pendiente_verificacion',

  creada_en           timestamptz NOT NULL DEFAULT now(),
  anonimizada_en      timestamptz,

  -- El alcance de RF-23, en el motor.
  CONSTRAINT cuenta_alcance_por_tipo CHECK (
    (tipo = 'administrador' AND tenant_id IS NOT NULL)
    OR (tipo IN ('usuario', 'super_admin') AND tenant_id IS NULL)
  ),

  -- Hace falta al menos un canal: es la unica forma de recuperar la cuenta
  -- (RF-24) y de verificarla (RF-19).
  CONSTRAINT cuenta_contacto_minimo
    CHECK (email IS NOT NULL OR telefono IS NOT NULL),

  CONSTRAINT cuenta_email_formato
    CHECK (email IS NULL OR email ~ '^[^@[:space:]]+@[^@[:space:]]+\.[^@[:space:]]+$'),

  -- E.164. Se guarda normalizado; el formateo para mostrar es del cliente.
  CONSTRAINT cuenta_telefono_formato
    CHECK (telefono IS NULL OR telefono ~ '^\+[1-9][0-9]{7,14}$'),

  -- Una cuenta anonimizada conserva la fila --los comprobantes de RF-34 la
  -- referencian-- pero ya no puede estar activa.
  CONSTRAINT cuenta_anonimizada_coherente
    CHECK (anonimizada_en IS NULL OR estado = 'eliminada')
);

-- Unicidad sobre lower(): 'Ana@x.com' y 'ana@x.com' son la misma persona, y
-- descubrirlo cuando ya existen dos cuentas es caro.
--
-- Los indices son PARCIALES, y ahi esta lo interesante: excluyen las cuentas
-- eliminadas. RF-25 anonimiza en vez de borrar, asi que la fila permanece; si
-- la unicidad fuera total, ese correo quedaria quemado para siempre y una
-- persona que se dio de baja no podria volver a registrarse. La anonimizacion
-- pone el email en NULL y el indice parcial deja el camino libre.
CREATE UNIQUE INDEX cuenta_email_uq
  ON plataforma.cuenta (lower(email))
  WHERE email IS NOT NULL AND estado <> 'eliminada';

CREATE UNIQUE INDEX cuenta_telefono_uq
  ON plataforma.cuenta (telefono)
  WHERE telefono IS NOT NULL AND estado <> 'eliminada';

-- Un administrador entra por su tenant; un super_admin no aparece aqui.
CREATE INDEX cuenta_por_tenant
  ON plataforma.cuenta (tenant_id)
  WHERE tenant_id IS NOT NULL;

COMMENT ON COLUMN plataforma.cuenta.tenant_id IS
  'Significa "administra este negocio", NO "reserva en el". Un usuario normal '
  'siempre lo tiene NULL aunque tenga reservas en veinte tenants: eso lo dice '
  'indice_reserva_global.';
COMMENT ON COLUMN plataforma.cuenta.password_hash IS
  'Argon2id (RNF-09). Nulo mientras la cuenta solo use magic link u OTP.';


-- -----------------------------------------------------------------------------
-- indice_reserva_global
-- -----------------------------------------------------------------------------
-- Existe por una razon puramente fisica. negocio.reserva esta particionada por
-- HASH (tenant_id), asi que "dame todas las reservas de esta cuenta" --que es
-- RF-02, y tiene 400 ms de presupuesto en RNF-02-- no tiene por donde podar:
-- obligaria a barrer las 64 particiones para descubrir que 62 estan vacias.
--
-- Esta tabla responde primero "a que tenants ir", y solo entonces se consultan
-- las particiones correctas. Dos consultas baratas en vez de un abanico ciego.
--
-- Es una denormalizacion consciente, y su costo es que hay que escribirla en la
-- MISMA transaccion que la reserva. Si se dejara para un trabajador asincrono,
-- una reserva recien creada no apareceria en el listado del usuario que acaba
-- de crearla, que es precisamente la lectura-de-lo-escrito que RNF-10 exige.

CREATE TABLE plataforma.indice_reserva_global (
  cuenta_id  uuid NOT NULL REFERENCES plataforma.cuenta (id),
  tenant_id  uuid NOT NULL REFERENCES plataforma.tenant (id),
  reserva_id uuid NOT NULL,
  creada_en  timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (cuenta_id, tenant_id, reserva_id)
);

-- El orden de lectura de RF-02: lo mas reciente primero.
CREATE INDEX indice_reserva_global_por_cuenta
  ON plataforma.indice_reserva_global (cuenta_id, creada_en DESC);

COMMENT ON TABLE plataforma.indice_reserva_global IS
  'Puntero cuenta -> tenant para evitar el abanico sobre las 64 particiones de '
  'negocio.reserva (RF-02 / RNF-02). Se escribe en la misma transaccion que la reserva.';
COMMENT ON COLUMN plataforma.indice_reserva_global.reserva_id IS
  'La FK compuesta (tenant_id, reserva_id) -> negocio.reserva se agrega en la '
  'migracion 0006, cuando esa tabla ya existe.';
