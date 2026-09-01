-- =============================================================================
-- 0010 - Negocio: outbox transaccional
-- =============================================================================
-- Evita la doble escritura hacia NATS, que es un problema sin solucion limpia
-- fuera de este patron.
--
-- Publicar despues del COMMIT puede fallar --el proceso muere, la red se cae,
-- NATS no responde-- y entonces la reserva existe y el evento no: nadie manda
-- la confirmacion y nadie se entera. Publicar ANTES del COMMIT es peor, porque
-- la transaccion todavia puede revertirse y queda un evento sobre una reserva
-- que nunca existio.
--
-- La salida es no publicar: escribir el evento en la MISMA transaccion que el
-- cambio, contra la misma base, con las mismas garantias. Si la reserva se
-- confirma, el evento tambien; si se revierte, tampoco queda. Un relay aparte
-- drena esta tabla hacia JetStream con reintentos, y la deduplicacion la hace
-- NATS con Nats-Msg-Id (ER-03).
--
-- La consecuencia es que la entrega es AL MENOS UNA VEZ, nunca exactamente una:
-- el relay puede publicar y morir antes de marcar la fila. Todo consumidor
-- tiene que ser idempotente. No es un defecto de esta tabla, es la unica
-- garantia que se puede dar sin una transaccion distribuida, que es justo lo
-- que ARQ-01 se niega a tener.
-- =============================================================================


CREATE TABLE negocio.outbox_evento (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL DEFAULT gen_random_uuid(),

  tipo         text        NOT NULL,
  payload      jsonb       NOT NULL,
  creado_en    timestamptz NOT NULL DEFAULT now(),

  -- NULL mientras no se haya publicado. Es el unico estado que el relay mira,
  -- y por eso es tambien el predicado de su indice.
  publicado_en timestamptz,

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),

  CONSTRAINT outbox_tipo_no_vacio
    CHECK (length(trim(tipo)) > 0)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'outbox_evento');

COMMENT ON TABLE negocio.outbox_evento IS
  'Eventos escritos en la misma transaccion que el cambio que los origina. Un '
  'relay los drena hacia NATS JetStream. Entrega al-menos-una-vez: todo '
  'consumidor debe ser idempotente.';

COMMENT ON COLUMN negocio.outbox_evento.id IS
  'Viaja como Nats-Msg-Id. Es lo que permite que JetStream descarte el '
  'duplicado cuando el relay republica algo que ya habia publicado antes de '
  'morir sin marcarlo.';


-- -----------------------------------------------------------------------------
-- Indice
-- -----------------------------------------------------------------------------
-- Uno solo, y parcial. El relay pregunta siempre lo mismo --que hay sin
-- publicar, en orden-- y las filas ya publicadas son la inmensa mayoria en
-- cuanto el sistema lleve un rato funcionando. Un indice completo las cargaria
-- todas para no volver a mirarlas nunca.

CREATE INDEX outbox_sin_publicar
  ON negocio.outbox_evento (creado_en)
  WHERE publicado_en IS NULL;


-- -----------------------------------------------------------------------------
-- RLS y privilegios
-- -----------------------------------------------------------------------------
-- La migracion 0007 recorrio las tablas que existian ENTONCES. Una tabla creada
-- despues no hereda nada de aquel recorrido: nace sin RLS y sin privilegios, y
-- si nadie lo hace aqui se queda asi. Es la clase de omision que no da ningun
-- error, solo deja de aislar.

ALTER TABLE negocio.outbox_evento ENABLE ROW LEVEL SECURITY;
ALTER TABLE negocio.outbox_evento FORCE  ROW LEVEL SECURITY;

CREATE POLICY aislamiento_tenant ON negocio.outbox_evento
  FOR ALL TO reservas_app, reservas_lectura
  USING (tenant_id = infra.tenant_actual())
  WITH CHECK (tenant_id = infra.tenant_actual());

-- Privilegios sobre el padre y sobre nada mas. Consultar una particion
-- directamente usa las politicas de esa particion --que no tiene ninguna-- y
-- devolveria las filas de todos los tenants del bucket. Ver la nota larga de la
-- migracion 0007.
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
    WHERE n.nspname = 'negocio' AND p.relname = 'outbox_evento'
  LOOP
    EXECUTE format(
      'REVOKE ALL ON negocio.%I FROM reservas_app, reservas_lectura',
      particion.relname);
  END LOOP;
END;
$$;

-- El nucleo inserta; el relay marca publicado_en. Ninguno de los dos borra: la
-- purga por antiguedad es trabajo de mantenimiento, y mientras tanto la tabla
-- es tambien el registro de que el evento se emitio.
GRANT SELECT, INSERT, UPDATE ON negocio.outbox_evento TO reservas_app;
GRANT SELECT                 ON negocio.outbox_evento TO reservas_lectura;
