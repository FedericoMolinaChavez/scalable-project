-- =============================================================================
-- 0002 - Tipos enumerados
-- =============================================================================
-- Se usan ENUM y no text + CHECK ni tablas de catalogo. El motivo es concreto:
-- un enum ocupa 4 bytes, se compara por entero y --lo que mas pesa aqui-- puede
-- aparecer en el predicado de un indice parcial, que es justo lo que la
-- restriccion de exclusion de reserva necesita.
--
-- El costo de la decision hay que aceptarlo con los ojos abiertos: ALTER TYPE
-- ADD VALUE agrega valores pero nunca los quita ni los renombra sin reescribir
-- la columna. Como las migraciones deben tolerar dos versiones del esquema en
-- linea a la vez, la regla de operacion es: primero se despliega el valor nuevo
-- del enum, despues el codigo que lo produce. Nunca al reves.
--
-- Los estados de aqui son los mismos de ER-01 y ER-03, con una sola unificacion
-- deliberada (ver actor_tipo).
-- =============================================================================


-- -----------------------------------------------------------------------------
-- plataforma (ER-02)
-- -----------------------------------------------------------------------------

CREATE TYPE plataforma.estado_tenant AS ENUM (
  'activo',
  'suspendido',
  'eliminado'      -- borrado logico e inmediato; la purga fisica va por lotes
);

CREATE TYPE plataforma.tipo_cuenta AS ENUM (
  'usuario',        -- sin tenant: reserva en los negocios que quiera
  'administrador',  -- acotado a UN tenant: es su dueno
  'super_admin'     -- sin tenant: alcance sobre todos
);

CREATE TYPE plataforma.estado_cuenta AS ENUM (
  'pendiente_verificacion',
  'activa',
  'suspendida',
  'eliminada'
);


-- -----------------------------------------------------------------------------
-- negocio (ER-01 y ER-03)
-- -----------------------------------------------------------------------------

-- Un solo enum para sede, servicio y recurso. En ER-01 aparecen como
-- (activa, inactiva) y (activo, inactivo), pero es la misma maquina de estados
-- y la concordancia gramatical no justifica tres tipos separados.
CREATE TYPE negocio.estado_catalogo AS ENUM (
  'activo',
  'inactivo'
);

CREATE TYPE negocio.estado_reserva AS ENUM (
  'pendiente',    -- ES el bloqueo de RF-27: lleva expira_en
  'confirmada',   -- el webhook de pago la promovio (RF-33)
  'en_curso',
  'completada',
  'cancelada',
  'no_show',
  'expirada'      -- el bloqueo vencio sin pago y el cupo se libero
);

CREATE TYPE negocio.tipo_excepcion AS ENUM (
  'feriado',
  'mantenimiento',
  'cierre'
);

-- ER-01 lo definia como (usuario, admin, agente, sistema) y ER-03 como
-- (usuario, administrador, agente, sistema, super_admin). Se toma el segundo:
-- si transicion_estado no puede distinguir a un super_admin de un
-- administrador, la traza de RF-28 pierde justo la informacion mas sensible.
CREATE TYPE negocio.actor_tipo AS ENUM (
  'usuario',
  'administrador',
  'agente',
  'sistema',
  'super_admin'
);

CREATE TYPE negocio.estado_espera AS ENUM (
  'en_espera',
  'notificada',
  'cumplida',
  'descartada'
);

CREATE TYPE negocio.estado_voucher AS ENUM (
  'activo',
  'eliminado'      -- RF-17 no permite modificar: solo crear o eliminar
);
