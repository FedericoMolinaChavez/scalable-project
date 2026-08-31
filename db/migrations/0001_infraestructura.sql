-- =============================================================================
-- 0001 - Infraestructura: esquemas, extensiones, roles y utilidades
-- =============================================================================
-- Esta migracion no crea ninguna tabla de negocio. Establece el terreno sobre
-- el que se apoyan todas las demas: donde viven las tablas, que extensiones
-- necesita el motor para sostener la invariante de RNF-10, quien puede tocar
-- que, y como se le dice a PostgreSQL "esta transaccion pertenece al tenant X".
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Esquemas
-- -----------------------------------------------------------------------------
-- Tres, y la frontera entre ellos es la misma que separa ER-01/ER-03 de ER-02:
--
--   plataforma : tablas globales, SIN tenant_id ni particionado. Una persona
--                tiene una sola cuenta aunque reserve en veinte negocios.
--   negocio    : tablas por tenant, particionadas por HASH (tenant_id) y con
--                Row-Level Security. Todo lo que un administrador posee.
--   infra      : funciones de soporte. No contiene datos.
--
-- Separar los esquemas no es cosmetico: hace que "esta tabla lleva tenant_id"
-- sea una propiedad verificable con una consulta al catalogo, y por eso la
-- migracion 0007 puede aplicar RLS a todo negocio.* sin listar tablas a mano.

CREATE SCHEMA IF NOT EXISTS plataforma;
CREATE SCHEMA IF NOT EXISTS negocio;
CREATE SCHEMA IF NOT EXISTS infra;

COMMENT ON SCHEMA plataforma IS
  'Tablas globales sin tenant_id: identidad, sesiones, agentes (ER-02).';
COMMENT ON SCHEMA negocio IS
  'Tablas por tenant, particionadas por HASH (tenant_id) y bajo RLS (ER-01, ER-03).';
COMMENT ON SCHEMA infra IS
  'Funciones de soporte: contexto de tenant, particionado, inmutabilidad.';


-- -----------------------------------------------------------------------------
-- Extensiones
-- -----------------------------------------------------------------------------
-- btree_gist es la unica que no es negociable. Sin ella, un indice GiST no sabe
-- comparar uuid con "=", y la restriccion de exclusion
--     EXCLUDE (tenant_id WITH =, recurso_id WITH =, periodo WITH &&)
-- no se puede crear. Es decir: sin btree_gist no hay RNF-10 en el motor, y la
-- decision estructural mas importante del diseno se cae.

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- pgcrypto aporta gen_random_uuid(). Ver la nota sobre UUID mas abajo: se usa
-- solo como red de seguridad para filas creadas fuera de la aplicacion.
CREATE EXTENSION IF NOT EXISTS pgcrypto;


-- -----------------------------------------------------------------------------
-- Roles
-- -----------------------------------------------------------------------------
-- Son roles de grupo NOLOGIN. Los usuarios de conexion reales los crea
-- CloudNativePG con credenciales que viven en sealed secrets; aqui solo se
-- define QUE puede hacer cada perfil, nunca COMO se autentica. Una contrasena
-- jamas debe entrar a una migracion versionada.
--
--   reservas_app     : nucleo de escritura y configuracion. Sujeto a RLS.
--   reservas_lectura : consulta y disponibilidad, contra replicas. Solo SELECT,
--                      tambien sujeto a RLS.
--   reservas_soporte : BYPASSRLS. Es la unica via del super_admin de RF-23 y de
--                      la purga asincrona de tenants borrados. Toda sesion con
--                      este rol es auditable por definicion (RNF-36): no existe
--                      razon legitima para usarlo desde la ruta de peticion.

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'reservas_app') THEN
    CREATE ROLE reservas_app NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'reservas_lectura') THEN
    CREATE ROLE reservas_lectura NOLOGIN;
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'reservas_soporte') THEN
    CREATE ROLE reservas_soporte NOLOGIN BYPASSRLS;
  END IF;
END;
$$;

GRANT USAGE ON SCHEMA plataforma, negocio, infra
  TO reservas_app, reservas_lectura, reservas_soporte;


-- -----------------------------------------------------------------------------
-- Contexto de tenant
-- -----------------------------------------------------------------------------
-- El contrato con la aplicacion, y conviene leerlo dos veces porque es el punto
-- donde el diseno de datos y el de despliegue se tocan:
--
--   BEGIN;
--   SELECT set_config('app.tenant_id', $1, true);   -- true = SET LOCAL
--   ... operaciones ...
--   COMMIT;
--
-- El tercer argumento en true es obligatorio, no una preferencia de estilo.
-- ARQ-01 pone PgBouncer en modo transaccion: una conexion de servidor se
-- devuelve al pool al terminar cada transaccion y la toma otro pod. Un
-- SET de sesion sobreviviria a ese cambio y el siguiente tenant heredaria el
-- contexto del anterior, que es exactamente la fuga que RNF-06 prohibe.
-- set_config(..., true) se revierte en el COMMIT y hace imposible ese escenario.
--
-- Sin contexto la funcion devuelve NULL, y como "tenant_id = NULL" nunca es
-- verdadero, las politicas de RLS no devuelven ninguna fila. Fail-closed: una
-- transaccion que olvida fijar el tenant no ve datos de nadie, en vez de verlos
-- de todos.

CREATE OR REPLACE FUNCTION infra.tenant_actual()
RETURNS uuid
LANGUAGE sql
STABLE
PARALLEL SAFE
AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid;
$$;

COMMENT ON FUNCTION infra.tenant_actual() IS
  'Tenant de la transaccion actual. NULL si no se fijo: las politicas de RLS '
  'no devuelven filas (fail-closed). Fijar siempre con set_config(..., true).';

GRANT EXECUTE ON FUNCTION infra.tenant_actual()
  TO reservas_app, reservas_lectura, reservas_soporte;


-- -----------------------------------------------------------------------------
-- Particionado
-- -----------------------------------------------------------------------------
-- 64 particiones HASH, fijas, para toda tabla de negocio.
--
-- El numero se elige una vez y cambiarlo despues obliga a reescribir la tabla,
-- asi que la eleccion importa. Con decenas de miles de tenants, 64 particiones
-- dejan cada una en un tamano que vacuum e indices manejan comodamente, sin
-- inflar el tiempo de planificacion: la ruta caliente siempre filtra por
-- tenant_id = $1, asi que el planificador poda 63 de las 64 antes de ejecutar.
--
-- Es potencia de dos a proposito. PostgreSQL admite modulos mixtos entre
-- particiones de una misma tabla, de modo que una particion se puede DETACH y
-- reemplazar por dos con el modulo duplicado. Duplicar solo la particion
-- caliente es posible; pasar de 64 a 100 no lo es.
--
-- Limite conocido: HASH reparte tenants uniformemente, no carga. Un tenant
-- muy grande convierte su particion en un punto caliente y ninguna cantidad de
-- particiones lo arregla. Si eso llega a pasar, la salida es sacar ese tenant a
-- su propia particion por LIST, no subir el numero de particiones HASH.

-- Es un PROCEDURE y no una funcion para poder invocarlo con CALL: una funcion
-- que devuelve void obliga a un SELECT, y cada llamada ensuciaria la salida de
-- la migracion con una fila vacia por tabla.
CREATE OR REPLACE PROCEDURE infra.particionar_hash(
  p_esquema     text,
  p_tabla       text,
  p_particiones int DEFAULT 64
)
LANGUAGE plpgsql
AS $$
DECLARE
  i int;
BEGIN
  FOR i IN 0 .. p_particiones - 1 LOOP
    EXECUTE format(
      'CREATE TABLE IF NOT EXISTS %I.%I PARTITION OF %I.%I '
      'FOR VALUES WITH (MODULUS %s, REMAINDER %s)',
      p_esquema, p_tabla || '_p' || lpad(i::text, 2, '0'),
      p_esquema, p_tabla,
      p_particiones, i
    );
  END LOOP;
END;
$$;

COMMENT ON PROCEDURE infra.particionar_hash(text, text, int) IS
  'Crea las particiones HASH de una tabla de negocio. 64 por defecto.';


-- -----------------------------------------------------------------------------
-- Validacion de zonas horarias
-- -----------------------------------------------------------------------------
-- tenant y sede guardan su zona horaria como texto, y ese texto se usa despues
-- para convertir cada regla de disponibilidad a instantes concretos. Un
-- 'America/Bogata' mal escrito no falla al guardarse: falla semanas mas tarde,
-- dentro del calculo de disponibilidad y en otro componente, que es el peor
-- lugar posible para enterarse.
--
-- No se puede validar con un CHECK directo. Consultar pg_timezone_names seria
-- una subconsulta --prohibida en CHECK-- y "now() AT TIME ZONE z" lanza una
-- excepcion en vez de devolver falso. Esta funcion atrapa esa excepcion y la
-- convierte en un booleano, que si sirve.
--
-- Va marcada IMMUTABLE con una salvedad honesta: la base de zonas horarias del
-- sistema puede cambiar entre versiones de tzdata, asi que estrictamente no lo
-- es. En la practica los identificadores solo se agregan, casi nunca se
-- retiran, y el riesgo real --una fila existente que deje de validar-- solo
-- afectaria a una revalidacion explicita.

CREATE OR REPLACE FUNCTION infra.zona_horaria_valida(p_zona text)
RETURNS boolean
LANGUAGE plpgsql
IMMUTABLE
PARALLEL SAFE
AS $$
BEGIN
  PERFORM timestamptz '2000-01-01 00:00:00+00' AT TIME ZONE p_zona;
  RETURN true;
EXCEPTION
  WHEN OTHERS THEN
    RETURN false;
END;
$$;

COMMENT ON FUNCTION infra.zona_horaria_valida(text) IS
  'Valida un identificador IANA contra la base de zonas horarias del motor. '
  'Usable en CHECK, a diferencia de now() AT TIME ZONE.';


-- -----------------------------------------------------------------------------
-- Inmutabilidad
-- -----------------------------------------------------------------------------
-- Varias tablas son append-only por requerimiento, no por convencion:
-- politica_version (RF-15), transicion_estado (RF-28) y mas adelante
-- evento_auditoria (RNF-36). "Append-only" solo significa algo si el motor lo
-- impide; si depende de que nadie escriba el UPDATE equivocado, es documentacion.
--
-- Se aplican dos capas, y son distintas a proposito:
--   1. REVOKE UPDATE, DELETE  -> el rol de la aplicacion no tiene el permiso.
--   2. Este disparador        -> cubre a cualquier rol que si lo tenga, incluido
--                                el dueno de la tabla en una sesion de soporte.
-- La primera es la que actua en produccion; la segunda es la que evita que una
-- correccion manual apresurada reescriba historia.

CREATE OR REPLACE FUNCTION infra.prohibir_mutacion()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION
    'La tabla %.% es append-only: no admite % (RF-15 / RF-28 / RNF-36).',
    TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'restrict_violation';
END;
$$;

COMMENT ON FUNCTION infra.prohibir_mutacion() IS
  'Disparador de sentencia que rechaza UPDATE y DELETE sobre tablas append-only.';


-- -----------------------------------------------------------------------------
-- Nota sobre las claves primarias
-- -----------------------------------------------------------------------------
-- Los identificadores son uuid y los genera la aplicacion en Go, no el motor,
-- y conviene que sean UUIDv7 (ordenados por tiempo) y no v4 (aleatorios).
--
-- La razon es fisica. Un uuid v4 cae en una hoja arbitraria del indice B-tree
-- en cada insercion: el arbol no tiene punto caliente, cada escritura ensucia
-- una pagina distinta, y sobre reserva --que recibe ~3.300 inserciones por
-- segundo segun RNF-03-- eso multiplica el trabajo de WAL y de vacuum. Un
-- UUIDv7 lleva el timestamp en los bits altos, asi que las inserciones se
-- concentran al final del indice, como haria un bigserial, pero sin volver el
-- identificador adivinable ni coordinar secuencias entre particiones.
--
-- gen_random_uuid() queda como DEFAULT solo para filas creadas por semillas o
-- por mano. La ruta caliente nunca deberia usarlo.
