-- =============================================================================
-- 0013 - Negocio: auditoria (RF-36 / RNF-36) y tarifas (RF-31)
-- =============================================================================
-- Dos tablas de ER-03 que no dependen del dinero de Stripe y que la superficie
-- de configuracion del administrador necesita: la que registra quien hizo que,
-- y la que permite que un precio dependa de cuando.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Tipos
-- -----------------------------------------------------------------------------

CREATE TYPE negocio.resultado_auditoria AS ENUM (
  'exito',
  'rechazo'
);

CREATE TYPE negocio.tipo_tarifa AS ENUM (
  'franja_horaria',
  'dia_semana',
  'temporada'
);


-- -----------------------------------------------------------------------------
-- evento_auditoria
-- -----------------------------------------------------------------------------
-- RF-36 y RNF-36: append-only, y se escribe en la MISMA transaccion que la
-- operacion que audita. Si no se puede auditar, la operacion no ocurre.
--
-- Vive en PostgreSQL y no en un bus, y esa es la decision que lo hace posible:
-- con un outbox y un relay la auditoria seria eventual, y "eventual" y
-- "condicion de exito" son incompatibles. Al estar en la misma transaccion no
-- hace falta ni outbox ni relay.
--
-- Se distingue de negocio.transicion_estado y la diferencia importa: aquella
-- cuenta la vida de UNA reserva y se le muestra al cliente; esta registra quien
-- hizo que en todo el sistema y es evidencia de seguridad. Fundirlas obligaria
-- a filtrar una traza de seguridad para poder ensenarsela a un cliente.

CREATE TABLE negocio.evento_auditoria (
  tenant_id             uuid NOT NULL REFERENCES plataforma.tenant (id),
  id                    uuid NOT NULL DEFAULT gen_random_uuid(),
  ocurrido_en           timestamptz NOT NULL DEFAULT now(),

  -- Quien. actor_id es nulo solo para el sistema, igual que en
  -- transicion_estado y por la misma razon: un barrido automatico no tiene
  -- identidad, y fingir una seria peor que admitirlo.
  actor_tipo            negocio.actor_tipo NOT NULL,
  actor_id              uuid,

  -- Y si actuo un agente, en nombre de quien y con que alcance (RF-13/RF-23).
  -- Las tres columnas van juntas: un agente sin cuenta impersonada no es un
  -- agente, es un actor sin sujeto.
  agente_id             uuid,
  cuenta_impersonada_id uuid,
  alcance_token         jsonb,

  -- Que. recurso_tipo es text y no un enum porque la lista crece con cada
  -- superficie nueva --sede, servicio, recurso, regla, politica, voucher,
  -- tarifa, cuenta, sesion-- y un ALTER TYPE por cada una seria una migracion
  -- que no puede correr dentro de una transaccion con el resto.
  accion                text NOT NULL,
  recurso_tipo          text NOT NULL,
  recurso_id            uuid,

  -- Como acabo. Un RECHAZO tambien se audita, y no es un detalle: la traza que
  -- solo registra lo que salio bien no sirve para investigar nada, porque un
  -- ataque es exactamente una sucesion de intentos que fallaron.
  resultado             negocio.resultado_auditoria NOT NULL,

  ip                    inet,
  dispositivo           text,

  -- ER-03 pone (tenant_id, id) como clave. PostgreSQL exige que la clave de
  -- particionado forme parte de toda restriccion unica, asi que ocurrido_en
  -- entra tambien. No cambia el significado: id sigue siendo unico por si
  -- mismo, y la clave sigue empezando por tenant_id.
  PRIMARY KEY (tenant_id, id, ocurrido_en),

  CONSTRAINT auditoria_actor_coherente
    CHECK ((actor_tipo = 'sistema') = (actor_id IS NULL)),

  -- Los tres campos del agente van o no van juntos. Un agente_id sin cuenta
  -- impersonada dejaria una fila que no responde la pregunta que RF-36 hace
  -- sobre ella: "en nombre de quien".
  CONSTRAINT auditoria_agente_completo
    CHECK ((agente_id IS NULL) = (cuenta_impersonada_id IS NULL)),

  CONSTRAINT auditoria_accion_no_vacia
    CHECK (length(trim(accion)) > 0 AND length(trim(recurso_tipo)) > 0)
) PARTITION BY RANGE (ocurrido_en);

COMMENT ON TABLE negocio.evento_auditoria IS
  'RF-36 / RNF-36. Append-only y atomica con la operacion que audita: si no se '
  'puede escribir, la operacion no ocurre. La retencion se aplica con DROP de '
  'las particiones vencidas, que es una operacion de catalogo y no un DELETE '
  'de millones de filas.';


-- Particionado por RANGE y no por HASH, al reves que el resto de negocio.*.
--
-- El motivo es la retencion. Sobre HASH(tenant_id), borrar lo mas viejo de dos
-- anos es un DELETE que recorre las 64 particiones y deja el espacio a merced
-- de vacuum; sobre RANGE(ocurrido_en) es un DROP TABLE de la particion del mes
-- vencido: instantaneo y sin fragmentacion. Se pierde la poda por tenant, y no
-- duele: esta tabla se consulta por rango de fechas (RF-36) y el tenant es el
-- filtro secundario que resuelve el indice.

CREATE OR REPLACE PROCEDURE infra.particionar_mes(
  p_esquema text,
  p_tabla   text,
  p_mes     date
)
LANGUAGE plpgsql
AS $$
DECLARE
  v_inicio date := date_trunc('month', p_mes)::date;
  v_fin    date := (date_trunc('month', p_mes) + interval '1 month')::date;
BEGIN
  EXECUTE format(
    'CREATE TABLE IF NOT EXISTS %I.%I PARTITION OF %I.%I FOR VALUES FROM (%L) TO (%L)',
    p_esquema, p_tabla || '_' || to_char(v_inicio, 'YYYYMM'),
    p_esquema, p_tabla, v_inicio, v_fin
  );
END;
$$;

COMMENT ON PROCEDURE infra.particionar_mes(text, text, date) IS
  'Crea la particion mensual de una tabla particionada por RANGE sobre una '
  'columna de fecha. Idempotente: se puede llamar en cada despliegue.';

-- Doce meses por delante desde el mes en curso. Un proceso de mantenimiento
-- llama a este procedimiento periodicamente; doce meses es margen de sobra para
-- que ese proceso pueda fallar unas cuantas veces sin consecuencias.
DO $$
DECLARE
  i int;
BEGIN
  FOR i IN 0 .. 11 LOOP
    CALL infra.particionar_mes(
      'negocio', 'evento_auditoria',
      (date_trunc('month', now()) + make_interval(months => i))::date);
  END LOOP;
END;
$$;

-- Y una particion por defecto, que en cualquier otra tabla seria discutible y
-- aqui es obligatoria.
--
-- RNF-36 dice que la auditoria es CONDICION DE EXITO de la operacion: si el
-- INSERT falla, la operacion se revierte. Sin particion por defecto, un mes sin
-- crear no produciria un hueco en la traza, produciria una caida del sistema
-- entero --ninguna accion critica podria completarse--, y el fallo llegaria
-- justo el dia 1 a las 00:00.
--
-- El coste hay que conocerlo: adjuntar despues la particion de un mes que ya
-- tiene filas en la de defecto exige moverlas primero, y ATTACH escanea la
-- tabla por defecto para comprobar que no queda ninguna. Es trabajo del proceso
-- de mantenimiento, no de la ruta caliente.
CREATE TABLE negocio.evento_auditoria_defecto
  PARTITION OF negocio.evento_auditoria DEFAULT;

COMMENT ON TABLE negocio.evento_auditoria_defecto IS
  'Red de seguridad: sin ella, un mes sin particion tumbaria toda accion '
  'critica, porque RNF-36 hace de la auditoria una condicion de exito.';


-- La consulta de RF-36: los eventos de un tenant en un rango, del mas reciente
-- al mas antiguo, filtrando por actor, accion o recurso.
CREATE INDEX auditoria_por_tenant
  ON negocio.evento_auditoria (tenant_id, ocurrido_en DESC);

CREATE INDEX auditoria_por_actor
  ON negocio.evento_auditoria (tenant_id, actor_id, ocurrido_en DESC)
  WHERE actor_id IS NOT NULL;

CREATE INDEX auditoria_por_recurso
  ON negocio.evento_auditoria (tenant_id, recurso_tipo, recurso_id, ocurrido_en DESC);


-- Append-only en el motor. Las dos capas de siempre: el disparador cubre a
-- cualquier rol y el REVOKE actua sobre el que si tendria el permiso.
CREATE TRIGGER evento_auditoria_inmutable
  BEFORE UPDATE OR DELETE ON negocio.evento_auditoria
  FOR EACH STATEMENT EXECUTE FUNCTION infra.prohibir_mutacion();


-- -----------------------------------------------------------------------------
-- tarifa
-- -----------------------------------------------------------------------------
-- RF-31. El precio base vive en servicio.precio_monto; esta tabla lo
-- sobrescribe cuando su condicion aplica, y existe aparte porque si es 1:N.
--
-- La reserva congela precio_cobrado al crearse, asi que publicar una tarifa
-- nueva jamas altera una reserva existente. Es la misma disciplina de snapshot
-- que politica_version, resuelta sin versionar el precio.

CREATE TABLE negocio.tarifa (
  tenant_id   uuid NOT NULL,
  id          uuid NOT NULL DEFAULT gen_random_uuid(),

  servicio_id uuid NOT NULL,
  tipo        negocio.tipo_tarifa NOT NULL,

  -- jsonb y sin esquema fijo porque el motor no la evalua: la interpreta quien
  -- calcula el precio, y su forma depende del tipo. Una franja horaria lleva
  -- horas, un dia de semana lleva un numero, una temporada lleva fechas.
  condicion   jsonb NOT NULL,

  monto       numeric(12,2) NOT NULL,

  -- Con dos tarifas que aplican a la vez gana la de prioridad mas alta. Sin
  -- este campo el resultado dependeria del orden de las filas, que no es un
  -- orden: es lo que el planificador decida ese dia.
  prioridad   int NOT NULL DEFAULT 0,

  creada_en   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id, servicio_id)
    REFERENCES negocio.servicio (tenant_id, id) ON DELETE CASCADE,

  CONSTRAINT tarifa_monto_no_negativo
    CHECK (monto >= 0),

  CONSTRAINT tarifa_condicion_es_objeto
    CHECK (jsonb_typeof(condicion) = 'object')
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'tarifa');

-- La consulta al calcular un precio: las tarifas de este servicio, de mayor a
-- menor prioridad.
CREATE INDEX tarifa_por_servicio
  ON negocio.tarifa (tenant_id, servicio_id, prioridad DESC);

COMMENT ON TABLE negocio.tarifa IS
  'RF-31. Sobrescribe servicio.precio_monto cuando su condicion aplica. La '
  'reserva congela el precio al crearse, asi que publicar una tarifa no toca '
  'ninguna reserva existente.';


-- -----------------------------------------------------------------------------
-- RLS y privilegios
-- -----------------------------------------------------------------------------
-- La migracion 0007 recorrio el catalogo y aplico la politica a todas las
-- tablas de negocio que existian ENTONCES. No es retroactiva: estas dos se
-- quedan fuera si no se hace aqui, y quedarse fuera significa que cualquier
-- tenant las ve enteras. Se repite el mismo bucle, acotado a las nuevas.

DO $$
DECLARE
  tabla text;
BEGIN
  FOREACH tabla IN ARRAY ARRAY['evento_auditoria', 'tarifa'] LOOP
    EXECUTE format('ALTER TABLE negocio.%I ENABLE ROW LEVEL SECURITY', tabla);
    EXECUTE format('ALTER TABLE negocio.%I FORCE  ROW LEVEL SECURITY', tabla);

    EXECUTE format(
      'CREATE POLICY aislamiento_tenant ON negocio.%I '
      'FOR ALL TO reservas_app, reservas_lectura '
      'USING (tenant_id = infra.tenant_actual()) '
      'WITH CHECK (tenant_id = infra.tenant_actual())',
      tabla
    );
  END LOOP;
END;
$$;

-- Los roles de aplicacion tienen privilegios sobre los PADRES y sobre ninguna
-- particion. Consultar una particion directamente usaria sus politicas --que no
-- tiene ninguna-- y devolveria filas de todos los tenants del bucket.
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
    WHERE n.nspname = 'negocio'
      AND p.relname IN ('evento_auditoria', 'tarifa')
  LOOP
    EXECUTE format(
      'REVOKE ALL ON negocio.%I FROM reservas_app, reservas_lectura',
      particion.relname);
  END LOOP;
END;
$$;

-- La auditoria es append-only tambien en los privilegios: sin UPDATE ni DELETE
-- ni siquiera para la aplicacion.
GRANT SELECT, INSERT         ON negocio.evento_auditoria TO reservas_app;
GRANT SELECT                 ON negocio.evento_auditoria TO reservas_lectura;

GRANT SELECT, INSERT, UPDATE, DELETE ON negocio.tarifa TO reservas_app;
GRANT SELECT                         ON negocio.tarifa TO reservas_lectura;

GRANT ALL ON negocio.evento_auditoria TO reservas_soporte;
GRANT ALL ON negocio.tarifa           TO reservas_soporte;
