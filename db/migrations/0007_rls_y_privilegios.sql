-- =============================================================================
-- 0007 - Row-Level Security y privilegios
-- =============================================================================
-- RNF-06 dice que ningun tenant puede ver datos de otro. Cumplirlo filtrando
-- por tenant_id en cada consulta es cumplirlo mientras nadie olvide el WHERE;
-- una sola consulta sin el, en cualquiera de los servicios, y el aislamiento
-- deja de existir sin que nada falle visiblemente.
--
-- RLS invierte eso: el filtro deja de ser algo que hay que acordarse de
-- escribir y pasa a ser algo que hay que tener permiso para evitar. Una
-- consulta sin WHERE devuelve las filas del tenant de la transaccion, y una
-- transaccion sin tenant fijado no devuelve ninguna.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Politicas sobre negocio.*
-- -----------------------------------------------------------------------------
-- Se recorre el catalogo en vez de listar tablas a mano, y eso es precisamente
-- lo que compra haber separado los esquemas en la migracion 0001: "toda tabla
-- de negocio lleva tenant_id" es verificable, asi que la politica se puede
-- aplicar por construccion y ninguna tabla futura se queda fuera por olvido.
--
-- FORCE, no solo ENABLE. Sin FORCE, el dueno de la tabla queda exento de sus
-- propias politicas, y como las migraciones y buena parte de las tareas de
-- mantenimiento corren como dueno, esa exencion se filtra a lugares donde no
-- deberia. El unico camino legitimo para saltarse RLS es reservas_soporte,
-- que tiene BYPASSRLS y por tanto deja rastro en la sesion.
--
-- USING gobierna que filas se ven; WITH CHECK, que filas se pueden escribir.
-- Ambas hacen falta: sin WITH CHECK, una transaccion del tenant A podria
-- INSERTAR una fila con tenant_id = B --no la veria despues, pero ya estaria
-- escrita-- y eso es corrupcion silenciosa, no un fallo.

DO $$
DECLARE
  tabla record;
BEGIN
  FOR tabla IN
    SELECT c.relname
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'negocio'
      AND c.relkind IN ('r', 'p')
      AND NOT c.relispartition
    ORDER BY c.relname
  LOOP
    EXECUTE format('ALTER TABLE negocio.%I ENABLE ROW LEVEL SECURITY', tabla.relname);
    EXECUTE format('ALTER TABLE negocio.%I FORCE  ROW LEVEL SECURITY', tabla.relname);

    EXECUTE format(
      'CREATE POLICY aislamiento_tenant ON negocio.%I '
      'FOR ALL TO reservas_app, reservas_lectura '
      'USING (tenant_id = infra.tenant_actual()) '
      'WITH CHECK (tenant_id = infra.tenant_actual())',
      tabla.relname
    );
  END LOOP;
END;
$$;


-- -----------------------------------------------------------------------------
-- Privilegios sobre negocio.*
-- -----------------------------------------------------------------------------
-- Aqui esta el agujero que RLS por si sola deja abierto, y no es evidente.
--
-- Consultar negocio.reserva pasa por las politicas del padre. Consultar
-- negocio.reserva_p07 DIRECTAMENTE usa las politicas de esa particion --que no
-- tiene ninguna-- y devuelve las filas de todos los tenants que caen en ese
-- bucket. Es una fuga completa de RNF-06 a la que se llega escribiendo el
-- nombre de una tabla.
--
-- El cierre es de privilegios, no de politicas: los roles de aplicacion tienen
-- permiso sobre los padres y sobre nada mas. Acceder por el padre no requiere
-- privilegios sobre las particiones, asi que no se pierde nada.

DO $$
DECLARE
  tabla record;
BEGIN
  FOR tabla IN
    SELECT c.relname, c.relispartition
    FROM pg_class c
    JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE n.nspname = 'negocio' AND c.relkind IN ('r', 'p')
  LOOP
    IF tabla.relispartition THEN
      EXECUTE format(
        'REVOKE ALL ON negocio.%I FROM reservas_app, reservas_lectura',
        tabla.relname);
    ELSE
      EXECUTE format(
        'GRANT SELECT, INSERT, UPDATE, DELETE ON negocio.%I TO reservas_app',
        tabla.relname);
      EXECUTE format(
        'GRANT SELECT ON negocio.%I TO reservas_lectura',
        tabla.relname);
    END IF;
  END LOOP;
END;
$$;

-- Append-only tambien en los privilegios. El disparador de la migracion 0001
-- cubre a cualquier rol; esto quita el permiso a los que si lo tendrian, que es
-- la capa que actua de verdad en produccion.
REVOKE UPDATE, DELETE ON negocio.politica_version   FROM reservas_app;
REVOKE UPDATE, DELETE ON negocio.transicion_estado  FROM reservas_app;

-- RF-17: un voucher se crea o se elimina, nunca se modifica. Pero
-- usos_actuales SI se actualiza en cada reserva, asi que UPDATE no se puede
-- revocar en bloque. Se acota por columna: el nucleo puede tocar el contador y
-- nada mas, y el resto de la fila queda tan inmutable como exige RF-17.
REVOKE UPDATE ON negocio.voucher FROM reservas_app;
GRANT  UPDATE (usos_actuales, estado) ON negocio.voucher TO reservas_app;


-- -----------------------------------------------------------------------------
-- Privilegios sobre plataforma.*
-- -----------------------------------------------------------------------------
-- Estas tablas no llevan RLS por tenant, y no es un descuido: la unidad de
-- aislamiento aqui es la CUENTA, no el negocio. Una politica
-- "tenant_id = infra.tenant_actual()" sobre indice_reserva_global romperia
-- justo su razon de existir, que es responder a traves de varios tenants a la
-- vez para el listado de RF-02.
--
-- El aislamiento por cuenta se define con el resto de ER-02 en la migracion
-- 0008, cuando exista sesion y este claro que identidad lleva la transaccion.
-- Hasta entonces lo sostiene la capa de autorizacion, y esta anotado como
-- deuda abierta y no como decision cerrada.

GRANT SELECT, INSERT, UPDATE ON plataforma.tenant TO reservas_app;
GRANT SELECT, INSERT, UPDATE ON plataforma.cuenta TO reservas_app;
GRANT SELECT, INSERT, DELETE ON plataforma.indice_reserva_global TO reservas_app;

GRANT SELECT ON plataforma.tenant                 TO reservas_lectura;
GRANT SELECT ON plataforma.cuenta                 TO reservas_lectura;
GRANT SELECT ON plataforma.indice_reserva_global  TO reservas_lectura;


-- -----------------------------------------------------------------------------
-- Soporte
-- -----------------------------------------------------------------------------
-- El super_admin de RF-23 y la purga por lotes de tenants eliminados. Es el
-- unico camino que atraviesa el aislamiento, y esta concentrado en un solo rol
-- a proposito: si algun dia hay que responder "quien pudo ver datos de varios
-- tenants", la respuesta es una consulta a pg_roles y no una auditoria de codigo.

GRANT ALL ON ALL TABLES IN SCHEMA plataforma, negocio TO reservas_soporte;
