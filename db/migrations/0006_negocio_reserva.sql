-- =============================================================================
-- 0006 - Negocio: reserva
-- =============================================================================
-- La tabla central del sistema. Todo lo anterior existe para que esta pueda
-- sostener una sola frase: dos personas no pueden ocupar el mismo recurso a la
-- misma hora, nunca, ni bajo un millon de solicitudes concurrentes.
--
-- Esa frase no se cumple con codigo de aplicacion. Se cumple con una
-- restriccion de exclusion, que es el unico mecanismo que sigue siendo cierto
-- cuando hay 45 pods escribiendo a la vez sin coordinarse entre ellos.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- reserva
-- -----------------------------------------------------------------------------

CREATE TABLE negocio.reserva (
  tenant_id            uuid NOT NULL,
  id                   uuid NOT NULL DEFAULT gen_random_uuid(),

  servicio_id          uuid NOT NULL,
  recurso_id           uuid NOT NULL,

  -- NULL = reserva como invitado (RF-01). El invitado consulta despues con el
  -- OTP de RF-02, por eso los datos de contacto son obligatorios en ese caso.
  cuenta_id            uuid,
  contacto_nombre      text,
  contacto_email       text,
  contacto_telefono    text,

  periodo              tstzrange NOT NULL,
  estado               negocio.estado_reserva NOT NULL DEFAULT 'pendiente',
  expira_en            timestamptz,

  -- Snapshot financiero: se congela al crear y ningun cambio de catalogo lo
  -- alcanza (RF-31). La moneda se copia de tenant por la misma razon: un
  -- comprobante ya emitido debe poder reproducirse aunque el negocio cambie
  -- de moneda despues (RF-34).
  precio_cobrado       numeric(12,2) NOT NULL,
  moneda               char(3) NOT NULL,

  politica_version_id  uuid NOT NULL,
  voucher_id           uuid,

  clave_idempotencia   text,

  creada_en            timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, servicio_id)
    REFERENCES negocio.servicio (tenant_id, id),
  FOREIGN KEY (tenant_id, recurso_id)
    REFERENCES negocio.recurso (tenant_id, id),
  FOREIGN KEY (tenant_id, politica_version_id)
    REFERENCES negocio.politica_version (tenant_id, id),
  FOREIGN KEY (tenant_id, voucher_id)
    REFERENCES negocio.voucher (tenant_id, id),
  FOREIGN KEY (cuenta_id)
    REFERENCES plataforma.cuenta (id),

  -- Limites [) explicitos, y esto es mas importante de lo que parece.
  -- Con limites cerrados por ambos lados, una reserva de 10:00 a 11:00 y otra
  -- de 11:00 a 12:00 comparten el instante 11:00 y el operador && las declara
  -- solapadas. Resultado: el motor rechazaria dos citas consecutivas
  -- perfectamente validas. Con [) el limite superior es abierto y encajan.
  CONSTRAINT reserva_periodo_valido CHECK (
    NOT isempty(periodo)
    AND lower(periodo) IS NOT NULL
    AND upper(periodo) IS NOT NULL
    AND lower_inc(periodo)
    AND NOT upper_inc(periodo)
  ),

  -- Si no hay cuenta, hace falta como contactar a quien reservo: sin esto una
  -- reserva de invitado no podria recibir su confirmacion ni su recordatorio.
  CONSTRAINT reserva_contacto_requerido CHECK (
    cuenta_id IS NOT NULL
    OR (contacto_nombre IS NOT NULL AND contacto_email IS NOT NULL)
  ),

  -- Una reserva pendiente ES un bloqueo (RF-27), y un bloqueo sin vencimiento
  -- es una denegacion de inventario permanente.
  CONSTRAINT reserva_pendiente_expira CHECK (
    estado <> 'pendiente' OR expira_en IS NOT NULL
  ),

  CONSTRAINT reserva_precio_no_negativo
    CHECK (precio_cobrado >= 0),

  CONSTRAINT reserva_moneda_iso
    CHECK (moneda ~ '^[A-Z]{3}$')
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'reserva');

COMMENT ON TABLE negocio.reserva IS
  'El bloqueo de RF-27 ES esta fila en estado pendiente con expira_en. No hay '
  'entidad de bloqueo aparte, y por eso no hay conversion bloqueo->reserva ni '
  'la carrera que esa conversion traeria.';

COMMENT ON COLUMN negocio.reserva.clave_idempotencia IS
  'AGREGADO respecto a ER-01. El modelo tenia idempotencia en el webhook, el '
  'reembolso y la notificacion, pero no en la creacion. Un agente de RNF-07 que '
  'reintenta un POST /reservas con timeout crearia dos bloqueos sobre horarios '
  'distintos y ninguno se liberaria hasta el TTL: denegacion de inventario '
  'por accidente, no por ataque. La clave la pone el cliente.';


-- -----------------------------------------------------------------------------
-- La invariante de RNF-10
-- -----------------------------------------------------------------------------
-- Se aplica particion por particion, y eso basta para garantizarla globalmente:
-- un recurso pertenece a un solo tenant, HASH(tenant_id) manda todas las filas
-- de ese tenant a la misma particion, y por lo tanto TODAS las reservas de un
-- recurso caen siempre en la misma. No hay solapamiento posible que quede
-- repartido entre dos particiones y se escape.
--
-- Sobre el predicado WHERE: solo pendiente y confirmada ocupan cupo. Una
-- cancelada o expirada permanece en la tabla --RF-28 necesita su historia--
-- pero no debe bloquear el horario a nadie mas.
--
-- Y aqui esta el detalle que explica por que existe un trabajador expirador:
-- el predicado NO puede decir "pendiente Y expira_en > now()". PostgreSQL
-- exige que el predicado de un indice sea inmutable, y now() no lo es --el
-- indice tendria que reevaluarse solo, lo que ningun indice hace--. La
-- consecuencia es que un bloqueo vencido sigue ocupando el cupo hasta que
-- alguien cambie su estado a 'expirada'. Ese alguien es el trabajador de
-- RF-27, y su frecuencia de ejecucion no es un detalle operativo: es lo que
-- determina cuanto tiempo un cupo libre parece ocupado.
--
-- Se falla cerrado a proposito. El error posible es rechazar una reserva que
-- podria haberse aceptado; el inaceptable seria aceptar dos.
--
-- (PostgreSQL 17 admite restricciones de exclusion declaradas sobre la tabla
--  particionada padre cuando incluyen la clave de particionado con "=". Se hace
--  por particion igualmente: funciona desde PG 13, no depende de la version del
--  motor que despliegue CloudNativePG, y el resultado fisico es identico
--  --un indice GiST por particion-- en ambos casos.)

DO $$
DECLARE
  particion record;
BEGIN
  FOR particion IN
    SELECT c.relname
    FROM pg_class c
    JOIN pg_inherits i ON i.inhrelid = c.oid
    JOIN pg_class p    ON p.oid = i.inhparent
    JOIN pg_namespace n ON n.oid = p.relnamespace
    WHERE n.nspname = 'negocio' AND p.relname = 'reserva'
    ORDER BY c.relname
  LOOP
    EXECUTE format(
      'ALTER TABLE negocio.%I ADD CONSTRAINT %I '
      'EXCLUDE USING gist (tenant_id WITH =, recurso_id WITH =, periodo WITH &&) '
      'WHERE (estado IN (''pendiente'', ''confirmada''))',
      particion.relname,
      particion.relname || '_sin_solape'
    );
  END LOOP;
END;
$$;

-- El indice GiST que crea esa restriccion tiene un segundo trabajo: es tambien
-- el que responde "que ocupa este recurso en esta ventana", que es la consulta
-- de disponibilidad de RF-26, el 90% del trafico segun RNF-03. La invariante y
-- la ruta de lectura mas caliente comparten estructura; no hay que mantener dos.


-- -----------------------------------------------------------------------------
-- Indices de reserva
-- -----------------------------------------------------------------------------
-- Cuatro, y ni uno mas. Cada indice adicional es trabajo en cada una de las
-- ~3.300 inserciones por segundo de RNF-03, asi que cada uno tiene que
-- justificar una consulta real del sistema.
--
-- (1) el GiST de la restriccion : disponibilidad, RF-26
-- (2) expirador                 : RF-27
-- (3) listado del usuario       : RF-02
-- (4) agenda del administrador  : RF-03

-- Parcial y estrecho. El expirador corre cada pocos segundos y solo le importan
-- las pendientes; sin el predicado, cada pasada barreria toda la tabla para
-- encontrar un punado de filas.
CREATE INDEX reserva_pendientes_por_vencer
  ON negocio.reserva (expira_en)
  WHERE estado = 'pendiente';

CREATE INDEX reserva_por_cuenta
  ON negocio.reserva (tenant_id, cuenta_id, creada_en DESC)
  WHERE cuenta_id IS NOT NULL;

-- lower(periodo) es inmutable, asi que sirve como expresion de indice. Ordena
-- la agenda por hora de inicio, que es como la lee un administrador.
CREATE INDEX reserva_agenda
  ON negocio.reserva (tenant_id, recurso_id, lower(periodo));

-- Idempotencia de creacion. Parcial porque la clave es opcional: los clientes
-- que no la mandan no pagan indice.
CREATE UNIQUE INDEX reserva_idempotencia_uq
  ON negocio.reserva (tenant_id, clave_idempotencia)
  WHERE clave_idempotencia IS NOT NULL;


-- Ahora que reserva existe, se cierra la FK del indice global.
ALTER TABLE plataforma.indice_reserva_global
  ADD CONSTRAINT indice_reserva_global_reserva_fk
  FOREIGN KEY (tenant_id, reserva_id)
  REFERENCES negocio.reserva (tenant_id, id);


-- -----------------------------------------------------------------------------
-- uso_voucher
-- -----------------------------------------------------------------------------
-- Materializa el consumo. voucher.usos_actuales es el contador que se compara
-- contra el limite; esta tabla es la evidencia de a que reserva fue cada uso, y
-- permite reconstruir el contador si alguna vez se desincroniza.

CREATE TABLE negocio.uso_voucher (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL DEFAULT gen_random_uuid(),

  voucher_id  uuid NOT NULL,
  reserva_id  uuid NOT NULL,
  aplicado_en timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, voucher_id)
    REFERENCES negocio.voucher (tenant_id, id),
  FOREIGN KEY (tenant_id, reserva_id)
    REFERENCES negocio.reserva (tenant_id, id),

  -- Una reserva consume como maximo un voucher. Es mas estricto que
  -- UNIQUE (voucher_id, reserva_id), que solo impediria aplicar el MISMO
  -- voucher dos veces a la misma reserva y dejaria pasar dos distintos.
  CONSTRAINT uso_voucher_una_por_reserva UNIQUE (tenant_id, reserva_id)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'uso_voucher');

CREATE INDEX uso_voucher_por_voucher
  ON negocio.uso_voucher (tenant_id, voucher_id);


-- -----------------------------------------------------------------------------
-- transicion_estado
-- -----------------------------------------------------------------------------
-- Historial append-only de RF-28. Distinto de evento_auditoria (RNF-36, viene
-- en la migracion 0010) y la diferencia importa: esta tabla cuenta la vida de
-- una reserva y se le muestra al usuario; aquella registra quien hizo que en
-- todo el sistema y es evidencia de seguridad. Fundirlas obligaria a filtrar
-- una traza de seguridad para poder mostrarsela a un cliente.

CREATE TABLE negocio.transicion_estado (
  tenant_id       uuid NOT NULL,
  id              uuid NOT NULL DEFAULT gen_random_uuid(),

  reserva_id      uuid NOT NULL,
  estado_anterior negocio.estado_reserva,   -- NULL en la creacion
  estado_nuevo    negocio.estado_reserva NOT NULL,
  actor_tipo      negocio.actor_tipo     NOT NULL,
  actor_id        uuid,
  motivo          text,
  ocurrida_en     timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, reserva_id)
    REFERENCES negocio.reserva (tenant_id, id),

  CONSTRAINT transicion_cambia_algo
    CHECK (estado_anterior IS DISTINCT FROM estado_nuevo),

  -- El sistema no tiene identidad; una persona o un agente si.
  CONSTRAINT transicion_actor_coherente
    CHECK ((actor_tipo = 'sistema') = (actor_id IS NULL))
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'transicion_estado');

CREATE INDEX transicion_por_reserva
  ON negocio.transicion_estado (tenant_id, reserva_id, ocurrida_en);

CREATE TRIGGER transicion_estado_inmutable
  BEFORE UPDATE OR DELETE ON negocio.transicion_estado
  FOR EACH STATEMENT EXECUTE FUNCTION infra.prohibir_mutacion();


-- -----------------------------------------------------------------------------
-- lista_espera
-- -----------------------------------------------------------------------------
-- RF-37. El turno se resuelve por orden de creada_en, no por prioridad ni por
-- pago: es lo unico que un usuario acepta como justo sin que haya que explicarlo.
--
-- recurso_id es opcional a proposito. "Quiero el martes a las 10 con quien sea"
-- es una espera mas util que una atada a un recurso concreto, y cubre mas casos
-- de liberacion.

CREATE TABLE negocio.lista_espera (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL DEFAULT gen_random_uuid(),

  servicio_id uuid NOT NULL,
  recurso_id  uuid,
  periodo     tstzrange NOT NULL,
  cuenta_id   uuid NOT NULL,
  estado      negocio.estado_espera NOT NULL DEFAULT 'en_espera',
  creada_en   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, servicio_id)
    REFERENCES negocio.servicio (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, recurso_id)
    REFERENCES negocio.recurso (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (cuenta_id)
    REFERENCES plataforma.cuenta (id),

  CONSTRAINT espera_periodo_valido
    CHECK (NOT isempty(periodo)
           AND lower(periodo) IS NOT NULL
           AND upper(periodo) IS NOT NULL)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'lista_espera');

-- La consulta del trabajador cuando se libera un cupo: quien espera por este
-- servicio en una ventana que solape, en orden de llegada.
CREATE INDEX espera_activa
  ON negocio.lista_espera
  USING gist (tenant_id, servicio_id, periodo)
  WHERE estado = 'en_espera';

-- Una misma cuenta no se anota dos veces por la misma franja.
CREATE UNIQUE INDEX espera_sin_duplicados
  ON negocio.lista_espera (tenant_id, cuenta_id, servicio_id, periodo)
  WHERE estado = 'en_espera';


-- -----------------------------------------------------------------------------
-- calificacion
-- -----------------------------------------------------------------------------
-- RF-20. Una por reserva, y solo sobre reservas completadas. Esa segunda regla
-- no cabe en un CHECK --exige mirar otra tabla-- y vive en el nucleo; el UNIQUE
-- si es del motor porque es la que se ataca desde fuera.

CREATE TABLE negocio.calificacion (
  tenant_id  uuid NOT NULL,
  id         uuid NOT NULL DEFAULT gen_random_uuid(),

  reserva_id uuid NOT NULL,
  puntaje    smallint NOT NULL,
  comentario text,
  creada_en  timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, reserva_id)
    REFERENCES negocio.reserva (tenant_id, id),

  CONSTRAINT calificacion_una_por_reserva
    UNIQUE (tenant_id, reserva_id),

  CONSTRAINT calificacion_puntaje_valido
    CHECK (puntaje BETWEEN 1 AND 5)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'calificacion');
