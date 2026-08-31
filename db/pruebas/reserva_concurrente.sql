-- =============================================================================
-- Guion de pgbench: N clientes peleando por el MISMO cupo
-- =============================================================================
-- Todos los clientes intentan reservar la Sala 1 en la misma franja horaria, a
-- la vez, sin coordinarse entre ellos. Es la version reducida de lo que ocurre
-- con 45 pods bajo la carga de RNF-03.
--
-- El resultado correcto es exactamente una fila. No "casi siempre una": una,
-- en todas las ejecuciones, sin importar el numero de clientes.
--
--   pgbench -n -f reserva_concurrente.sql -c 100 -j 8 -t 10 ...
--
-- ON CONFLICT DO NOTHING absorbe la violacion de exclusion, que es lo mismo
-- que hace el nucleo al devolver "ese horario ya no esta disponible" en el
-- flujo alternativo 3 de RF-01. Sin el, pgbench abortaria cada cliente que
-- pierde la carrera, que es justamente lo que se espera que pase.
-- =============================================================================

BEGIN;

SELECT set_config('app.tenant_id',
                  '11111111-1111-1111-1111-111111111111', true);

INSERT INTO negocio.reserva (
  tenant_id, id,
  servicio_id, recurso_id,
  contacto_nombre, contacto_email,
  periodo, estado, expira_en,
  precio_cobrado, moneda,
  politica_version_id
) VALUES (
  '11111111-1111-1111-1111-111111111111',
  gen_random_uuid(),
  '33333333-3333-3333-3333-333333333333',
  '44444444-4444-4444-4444-444444444444',
  'Cliente concurrente',
  'concurrente@ejemplo.test',
  tstzrange('2026-09-07 10:00:00-05', '2026-09-07 11:00:00-05', '[)'),
  'pendiente',
  now() + interval '10 minutes',
  80000.00,
  'COP',
  '55555555-5555-5555-5555-555555555555'
)
ON CONFLICT DO NOTHING;

COMMIT;
