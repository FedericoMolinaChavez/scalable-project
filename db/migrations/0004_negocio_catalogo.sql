-- =============================================================================
-- 0004 - Negocio: catalogo y disponibilidad
-- =============================================================================
-- sede, servicio, recurso, servicio_recurso, regla_disponibilidad y
-- excepcion_calendario. Todo lo que define QUE se puede reservar y CUANDO,
-- sin que ninguna hora concreta llegue a existir como fila.
--
-- Dos patrones se repiten en todas las tablas de este esquema y no se vuelven
-- a explicar mas abajo:
--
--   PRIMARY KEY (tenant_id, id)
--     PostgreSQL exige que toda clave unica de una tabla particionada incluya
--     la clave de particionado. No es un impuesto: es lo que hace que la
--     unicidad sea comprobable dentro de una sola particion.
--
--   FOREIGN KEY (tenant_id, x_id) REFERENCES ... (tenant_id, id)
--     Las FK son compuestas y arrastran tenant_id. El efecto es que una
--     referencia entre tenants distintos no es "un error que hay que evitar":
--     es una fila que el motor rechaza. RNF-06 deja de depender de que la capa
--     de aplicacion recuerde filtrar.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- sede
-- -----------------------------------------------------------------------------

CREATE TABLE negocio.sede (
  tenant_id    uuid NOT NULL REFERENCES plataforma.tenant (id),
  id           uuid NOT NULL DEFAULT gen_random_uuid(),

  nombre       text NOT NULL,
  zona_horaria text NOT NULL,
  direccion    text,
  estado       negocio.estado_catalogo NOT NULL DEFAULT 'activo',
  creada_en    timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  CONSTRAINT sede_zona_horaria_valida
    CHECK (infra.zona_horaria_valida(zona_horaria))
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'sede');

COMMENT ON COLUMN negocio.sede.zona_horaria IS
  'Se repite respecto a tenant a proposito: una cadena puede operar en varios '
  'husos. Cuando coinciden, esta columna gana igual, para que el calculo de '
  'disponibilidad tenga una sola fuente y no un fallback condicional.';


-- -----------------------------------------------------------------------------
-- servicio
-- -----------------------------------------------------------------------------
-- Define QUE se reserva, cuanto dura y cuanto cuesta.
--
-- El precio vive aqui y no en una tabla aparte. Es 1:1, no se versiona --la
-- reserva guarda su propio snapshot en precio_cobrado-- y sacarlo a otra tabla
-- solo agregaria un join en la consulta mas caliente del sistema (RF-26). Las
-- tarifas diferenciadas si son 1:N y si tienen tabla propia (migracion 0009).

CREATE TABLE negocio.servicio (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL DEFAULT gen_random_uuid(),

  sede_id       uuid NOT NULL,
  nombre        text NOT NULL,
  descripcion   text,
  duracion_min  int  NOT NULL,
  precio_monto  numeric(12,2) NOT NULL,
  estado        negocio.estado_catalogo NOT NULL DEFAULT 'activo',
  creado_en     timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, sede_id) REFERENCES negocio.sede (tenant_id, id),

  -- Cota superior de 24 h: mas alla de eso ya no es una cita sino una estadia,
  -- y el modelo de ocupacion por franja deja de ser el correcto.
  CONSTRAINT servicio_duracion_valida
    CHECK (duracion_min > 0 AND duracion_min <= 1440),

  -- Cero se admite: un servicio gratuito sigue ocupando un recurso.
  CONSTRAINT servicio_precio_no_negativo
    CHECK (precio_monto >= 0)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'servicio');

CREATE INDEX servicio_por_sede
  ON negocio.servicio (tenant_id, sede_id)
  WHERE estado = 'activo';

COMMENT ON COLUMN negocio.servicio.precio_monto IS
  'Precio base. La moneda NO esta aqui: sale de tenant.moneda. La reserva '
  'congela ambos al crearse (RF-31).';


-- -----------------------------------------------------------------------------
-- recurso
-- -----------------------------------------------------------------------------
-- Define QUE se ocupa. Capacidad 1, por decision de modelo: sin aforo.
--
-- Esa decision es la que hace posible expresar "nunca sobrevender" como una
-- restriccion de exclusion. Con aforo N la invariante deja de ser "no hay dos
-- filas solapadas" y pasa a ser "no hay mas de N filas solapadas", que ningun
-- indice sabe comprobar y que obligaria a contar bajo bloqueo. Un espacio con
-- diez puestos se modela como diez recursos.

CREATE TABLE negocio.recurso (
  tenant_id  uuid NOT NULL,
  id         uuid NOT NULL DEFAULT gen_random_uuid(),

  sede_id    uuid NOT NULL,
  nombre     text NOT NULL,
  estado     negocio.estado_catalogo NOT NULL DEFAULT 'activo',
  creado_en  timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, sede_id) REFERENCES negocio.sede (tenant_id, id)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'recurso');

CREATE INDEX recurso_por_sede
  ON negocio.recurso (tenant_id, sede_id)
  WHERE estado = 'activo';

COMMENT ON TABLE negocio.recurso IS
  'Dueno del calendario. Capacidad 1: es sobre recurso_id que actua la '
  'restriccion de exclusion de reserva (RNF-10).';


-- -----------------------------------------------------------------------------
-- servicio_recurso
-- -----------------------------------------------------------------------------
-- Que recursos pueden prestar que servicio. La eleccion concreta la hace el
-- motor de disponibilidad al momento de reservar, no el cliente.

CREATE TABLE negocio.servicio_recurso (
  tenant_id   uuid NOT NULL,
  servicio_id uuid NOT NULL,
  recurso_id  uuid NOT NULL,

  PRIMARY KEY (tenant_id, servicio_id, recurso_id),

  FOREIGN KEY (tenant_id, servicio_id)
    REFERENCES negocio.servicio (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, recurso_id)
    REFERENCES negocio.recurso (tenant_id, id) ON DELETE CASCADE
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'servicio_recurso');

-- El sentido inverso: "que servicios puede prestar este recurso", que es la
-- direccion que usa la agenda del administrador.
CREATE INDEX servicio_recurso_por_recurso
  ON negocio.servicio_recurso (tenant_id, recurso_id);


-- -----------------------------------------------------------------------------
-- regla_disponibilidad
-- -----------------------------------------------------------------------------
-- El horario recurrente del recurso. Los slots NO se materializan: la
-- disponibilidad es un calculo --reglas, menos excepciones, menos reservas--
-- y no una tabla que haya que mantener sincronizada ni decidir hasta que fecha
-- generar.
--
-- Dos detalles que parecen menores y no lo son:
--
-- hora_inicio y hora_fin son `time`, sin zona. Una regla dice "los lunes de 9
-- a 17" en hora local del negocio, y eso es una afirmacion sobre el reloj de
-- pared, no sobre un instante. Guardarla como timestamptz la romperia cada
-- cambio de horario de verano: la sede seguiria abriendo a las 9, pero la
-- regla diria 8 o 10. La zona sale de sede y la conversion ocurre al calcular.
--
-- dia_semana usa 0 = domingo, para coincidir con extract(dow) de PostgreSQL y
-- con time.Weekday de Go. ISO-8601 empieza en lunes; mezclarlos produce un
-- desfase de un dia que solo se nota en produccion.

CREATE TABLE negocio.regla_disponibilidad (
  tenant_id      uuid NOT NULL,
  id             uuid NOT NULL DEFAULT gen_random_uuid(),

  recurso_id     uuid     NOT NULL,
  dia_semana     smallint NOT NULL,
  hora_inicio    time     NOT NULL,
  hora_fin       time     NOT NULL,
  vigente_desde  date,
  vigente_hasta  date,

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, recurso_id)
    REFERENCES negocio.recurso (tenant_id, id) ON DELETE CASCADE,

  CONSTRAINT regla_dia_valido
    CHECK (dia_semana BETWEEN 0 AND 6),

  -- Una franja que cruza medianoche (22:00-02:00) se modela como DOS reglas,
  -- una por dia. Permitir hora_fin < hora_inicio como "cruza el dia" obligaria
  -- a que cada consulta de disponibilidad distinguiera los dos casos, y esa
  -- rama es exactamente donde se cuelan los errores de calendario.
  CONSTRAINT regla_franja_valida
    CHECK (hora_fin > hora_inicio),

  CONSTRAINT regla_vigencia_valida
    CHECK (vigente_hasta IS NULL OR vigente_desde IS NULL
           OR vigente_hasta >= vigente_desde)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'regla_disponibilidad');

-- La lectura de RF-26: todas las reglas de un recurso, ordenadas por dia.
CREATE INDEX regla_por_recurso
  ON negocio.regla_disponibilidad (tenant_id, recurso_id, dia_semana);

COMMENT ON TABLE negocio.regla_disponibilidad IS
  'Horario recurrente. La disponibilidad se calcula desde aqui menos '
  'excepcion_calendario menos reserva: no existe tabla de slots.';


-- -----------------------------------------------------------------------------
-- excepcion_calendario
-- -----------------------------------------------------------------------------
-- Lo que rompe la regla: feriados, mantenimiento, cierres. Aqui si se usa
-- tstzrange, porque una excepcion es un intervalo concreto en el tiempo --el 25
-- de diciembre de este ano-- y no un patron recurrente.
--
-- Puede aplicar a un recurso o a una sede entera. Con sede_id, cubre todos sus
-- recursos sin tener que enumerarlos.

CREATE TABLE negocio.excepcion_calendario (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL DEFAULT gen_random_uuid(),

  recurso_id  uuid,
  sede_id     uuid,
  periodo     tstzrange NOT NULL,
  tipo        negocio.tipo_excepcion NOT NULL,
  motivo      text,
  creada_en   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  -- MATCH SIMPLE (el comportamiento por defecto): si recurso_id es NULL la FK
  -- no se comprueba, que es justo lo que se quiere para el caso de sede.
  FOREIGN KEY (tenant_id, recurso_id)
    REFERENCES negocio.recurso (tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, sede_id)
    REFERENCES negocio.sede (tenant_id, id) ON DELETE CASCADE,

  -- Exactamente uno de los dos: una excepcion sin destino no aplica a nada, y
  -- con ambos seria ambigua sobre cual manda.
  CONSTRAINT excepcion_alcance_unico
    CHECK ((recurso_id IS NOT NULL) <> (sede_id IS NOT NULL)),

  CONSTRAINT excepcion_periodo_valido
    CHECK (NOT isempty(periodo)
           AND lower(periodo) IS NOT NULL
           AND upper(periodo) IS NOT NULL)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'excepcion_calendario');

-- GiST porque la pregunta siempre es de solapamiento: "que excepciones tocan
-- esta ventana". Un B-tree sobre las fechas no responde eso sin escanear.
CREATE INDEX excepcion_por_recurso
  ON negocio.excepcion_calendario
  USING gist (tenant_id, recurso_id, periodo)
  WHERE recurso_id IS NOT NULL;

CREATE INDEX excepcion_por_sede
  ON negocio.excepcion_calendario
  USING gist (tenant_id, sede_id, periodo)
  WHERE sede_id IS NOT NULL;
