-- =============================================================================
-- 0005 - Negocio: politica de cancelacion y vouchers
-- =============================================================================
-- Estas dos tablas pertenecen a ER-03 y podrian esperar, pero no pueden: la
-- reserva las referencia por clave foranea. politica_version_id es NOT NULL en
-- reserva, asi que sin esta migracion la siguiente no compila.
--
-- Lo demas de ER-03 --tarifa, pago, reembolso, comprobante, notificaciones,
-- auditoria, outbox, metricas-- entra en las migraciones 0009 y siguientes.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- politica_version
-- -----------------------------------------------------------------------------
-- INMUTABLE por requerimiento (RF-15). No se edita ni se borra: publicar una
-- politica nueva es insertar una version nueva.
--
-- La reserva guarda politica_version_id al crearse, y ahi esta todo el asunto.
-- Sin versionado, un negocio podria endurecer su politica de cancelacion el
-- martes y aplicarsela a quien reservo el lunes bajo otras condiciones. Con
-- versionado, esa reserva sigue apuntando a la version que estaba vigente
-- cuando se hizo, y ningun cambio posterior puede alcanzarla.
--
-- Notese que el mecanismo elegido es versionado y no snapshot, al reves que el
-- precio. La diferencia es el uso: el precio se lee una vez y se congela como
-- numero; la politica se evalua cada vez que alguien intenta cancelar, y
-- necesita seguir siendo una entidad consultable, con sus reglas completas.

CREATE TABLE negocio.politica_version (
  tenant_id                uuid NOT NULL,
  id                       uuid NOT NULL DEFAULT gen_random_uuid(),

  servicio_id              uuid,           -- NULL = politica por defecto del tenant
  version                  int  NOT NULL,
  rango_cancelacion_horas  int  NOT NULL,
  rango_modificacion_horas int  NOT NULL,
  penalidad_pct            numeric(5,2),
  vigente_desde            timestamptz NOT NULL DEFAULT now(),
  creada_en                timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, servicio_id)
    REFERENCES negocio.servicio (tenant_id, id),

  CONSTRAINT politica_rangos_no_negativos
    CHECK (rango_cancelacion_horas >= 0 AND rango_modificacion_horas >= 0),

  CONSTRAINT politica_penalidad_valida
    CHECK (penalidad_pct IS NULL OR (penalidad_pct >= 0 AND penalidad_pct <= 100)),

  CONSTRAINT politica_version_positiva
    CHECK (version > 0)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'politica_version');

-- COALESCE con el uuid nulo porque en un indice UNIQUE los NULL no colisionan
-- entre si: sin esto, la politica por defecto del tenant (servicio_id NULL)
-- podria repetir numero de version cuantas veces quisiera.
CREATE UNIQUE INDEX politica_version_uq
  ON negocio.politica_version (
    tenant_id,
    COALESCE(servicio_id, '00000000-0000-0000-0000-000000000000'::uuid),
    version
  );

-- La consulta de RF-01: "cual es la version vigente ahora para este servicio".
CREATE INDEX politica_vigente
  ON negocio.politica_version (
    tenant_id,
    COALESCE(servicio_id, '00000000-0000-0000-0000-000000000000'::uuid),
    vigente_desde DESC
  );

-- Append-only en el motor, no en la convencion. Ver infra.prohibir_mutacion.
CREATE TRIGGER politica_version_inmutable
  BEFORE UPDATE OR DELETE ON negocio.politica_version
  FOR EACH STATEMENT EXECUTE FUNCTION infra.prohibir_mutacion();

COMMENT ON TABLE negocio.politica_version IS
  'Inmutable (RF-15). Publicar una politica nueva = INSERT de una version nueva. '
  'Una reserva existente jamas cambia de politica.';


-- -----------------------------------------------------------------------------
-- voucher
-- -----------------------------------------------------------------------------
-- RF-17: crear o eliminar, nunca modificar. Cambiarle las condiciones a un
-- voucher ya circulando es cambiarselas a quien ya lo recibio, y si esta en una
-- promocion el impacto es al negocio. El estado 'eliminado' es un borrado
-- logico; la fila permanece porque uso_voucher la referencia.
--
-- Solo porcentaje, nunca monto fijo. Es una restriccion de alcance del proyecto
-- y esta en el CHECK para que no se filtre por accidente.

CREATE TABLE negocio.voucher (
  tenant_id     uuid NOT NULL,
  id            uuid NOT NULL DEFAULT gen_random_uuid(),

  codigo        text    NOT NULL,
  porcentaje    numeric(5,2) NOT NULL,
  limite_usos   int,
  usos_actuales int     NOT NULL DEFAULT 0,
  caduca_en     timestamptz,
  alcance       jsonb,
  estado        negocio.estado_voucher NOT NULL DEFAULT 'activo',
  creado_en     timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),

  CONSTRAINT voucher_porcentaje_valido
    CHECK (porcentaje > 0 AND porcentaje <= 100),

  -- Todo voucher tiene un final: o se agota o caduca. Sin esto, un voucher del
  -- 50% sin limite ni fecha es una fuga de ingresos abierta para siempre.
  CONSTRAINT voucher_tiene_limite
    CHECK (limite_usos IS NOT NULL OR caduca_en IS NOT NULL),

  CONSTRAINT voucher_limite_positivo
    CHECK (limite_usos IS NULL OR limite_usos > 0),

  -- Red de seguridad del consumo atomico. El UPDATE condicional que lo aplica
  -- ya impide pasarse; esto garantiza que ninguna otra ruta de escritura pueda.
  CONSTRAINT voucher_usos_dentro_del_limite
    CHECK (usos_actuales >= 0
           AND (limite_usos IS NULL OR usos_actuales <= limite_usos))
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'voucher');

-- El codigo no se libera al eliminar el voucher. Reutilizarlo confundiria la
-- traza de RNF-36: dos promociones distintas con el mismo codigo en el historial.
CREATE UNIQUE INDEX voucher_codigo_uq
  ON negocio.voucher (tenant_id, upper(codigo));

COMMENT ON COLUMN negocio.voucher.usos_actuales IS
  'Se incrementa en la MISMA transaccion que la reserva, con '
  'UPDATE ... WHERE (limite_usos IS NULL OR usos_actuales < limite_usos): '
  'cero filas afectadas significa agotado y aborta la reserva. '
  'Punto de contencion conocido: un voucher viral serializa las escrituras '
  'sobre esta fila y puede salirse del presupuesto de RNF-01. Si eso pasa, la '
  'salida es repartir el cupo en N filas hijas, no relajar la atomicidad.';

COMMENT ON COLUMN negocio.voucher.alcance IS
  'jsonb: restringe a sedes o servicios concretos. Sin esquema fijo porque el '
  'motor no lo evalua; lo interpreta el nucleo al validar la reserva.';
