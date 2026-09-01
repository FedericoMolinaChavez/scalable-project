-- =============================================================================
-- 0009 - Negocio: una regla de disponibilidad no se repite
-- =============================================================================
-- regla_disponibilidad era la unica tabla de la semilla que se insertaba sin
-- id explicito Y sin clave unica natural. Su PRIMARY KEY es (tenant_id, id) con
-- id generado, asi que el ON CONFLICT DO NOTHING de la semilla no tenia contra
-- que chocar: no habia conflicto posible, y reaplicarla duplicaba las cinco
-- reglas en silencio.
--
-- El sintoma no fue un error, que es lo que lo hace peor: la disponibilidad
-- empezo a devolver DIECISEIS franjas donde la semilla define ocho. El CTE
-- `ventanas` une contra las reglas, dos reglas identicas producen dos ventanas
-- identicas, y de ahi salen dos franjas por cada hueco. Nadie se entera hasta
-- que cuenta.
--
-- Dos reglas identicas para el mismo recurso no significan nada distinto de
-- una: es un duplicado por definicion. Asi que lo impide el motor, como el
-- resto de las cosas que en este esquema no pueden pasar.
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Primero limpiar lo que ya se colo
-- -----------------------------------------------------------------------------
-- Sobre una base recien creada esto no borra nada: no hay duplicados que
-- limpiar. Existe para las bases de desarrollo que ya los tienen, donde anadir
-- la restriccion sin mas fallaria.
--
-- Se conserva la mas antigua de cada grupo. Da igual cual sobreviva --son
-- identicas-- pero elegir por ctid seria elegir por posicion fisica, que cambia
-- con un VACUUM y haria el resultado distinto en cada base.

DELETE FROM negocio.regla_disponibilidad r
WHERE EXISTS (
  SELECT 1
  FROM negocio.regla_disponibilidad otra
  WHERE otra.tenant_id     = r.tenant_id
    AND otra.recurso_id    = r.recurso_id
    AND otra.dia_semana    = r.dia_semana
    AND otra.hora_inicio   = r.hora_inicio
    AND otra.hora_fin      = r.hora_fin
    AND otra.vigente_desde IS NOT DISTINCT FROM r.vigente_desde
    AND otra.vigente_hasta IS NOT DISTINCT FROM r.vigente_hasta
    AND otra.id < r.id
);


-- -----------------------------------------------------------------------------
-- Y despues impedir que vuelva a pasar
-- -----------------------------------------------------------------------------
-- Incluye la vigencia: "los lunes de 9 a 17 hasta marzo" y "los lunes de 9 a 17
-- desde abril" son dos reglas legitimas y distintas, y una restriccion que solo
-- mirara dia y horas las declararia duplicadas.
--
-- COALESCE sobre las fechas porque en un indice UNIQUE los NULL no colisionan
-- entre si: sin eso, "sin vigencia definida" se podria repetir cuantas veces
-- quisiera, que es exactamente el caso que rompio la semilla. Es el mismo truco
-- que ya usa politica_version_uq en la migracion 0005.
--
-- Las fechas centinela estan fuera de cualquier calendario real, asi que no
-- pueden chocar con una vigencia de verdad.

CREATE UNIQUE INDEX regla_disponibilidad_uq
  ON negocio.regla_disponibilidad (
    tenant_id,
    recurso_id,
    dia_semana,
    hora_inicio,
    hora_fin,
    COALESCE(vigente_desde, '-infinity'::date),
    COALESCE(vigente_hasta,  'infinity'::date)
  );

COMMENT ON INDEX negocio.regla_disponibilidad_uq IS
  'Una regla no se repite. Dos identicas no significan nada distinto de una, '
  'pero duplican las franjas que calcula RF-26 sin dar ningun error.';
