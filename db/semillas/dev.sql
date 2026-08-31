-- =============================================================================
-- Semilla de desarrollo
-- =============================================================================
-- SOLO PARA ENTORNOS LOCALES. Crea un rol de conexion con contrasena en texto
-- plano y un tenant con identificadores fijos. Nunca se aplica en produccion:
-- alli los usuarios de conexion los crea CloudNativePG desde sealed secrets.
--
-- Los UUID son fijos a proposito, para que las pruebas puedan referenciarlos
-- sin tener que descubrirlos primero.
-- =============================================================================

-- Rol de conexion de desarrollo. Hereda de reservas_app, asi que queda sujeto
-- a RLS: es lo que permite que las pruebas ejerciten el aislamiento de verdad
-- y no por el atajo de conectarse como superusuario.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_dev') THEN
    CREATE ROLE app_dev LOGIN PASSWORD 'dev';
  END IF;
END;
$$;

GRANT reservas_app TO app_dev;
ALTER ROLE app_dev SET search_path = negocio, plataforma, infra, public;


-- -----------------------------------------------------------------------------
-- Un tenant con lo minimo para poder reservar
-- -----------------------------------------------------------------------------

INSERT INTO plataforma.tenant (id, identificador, nombre, moneda, zona_horaria)
VALUES ('11111111-1111-1111-1111-111111111111',
        'estudio-demo', 'Estudio Demo', 'COP', 'America/Bogota')
ON CONFLICT (id) DO NOTHING;

INSERT INTO negocio.sede (tenant_id, id, nombre, zona_horaria, direccion)
VALUES ('11111111-1111-1111-1111-111111111111',
        '22222222-2222-2222-2222-222222222222',
        'Sede Centro', 'America/Bogota', 'Calle 1 # 2-3')
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.servicio
  (tenant_id, id, sede_id, nombre, duracion_min, precio_monto)
VALUES ('11111111-1111-1111-1111-111111111111',
        '33333333-3333-3333-3333-333333333333',
        '22222222-2222-2222-2222-222222222222',
        'Sesion de una hora', 60, 80000.00)
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.recurso (tenant_id, id, sede_id, nombre)
VALUES ('11111111-1111-1111-1111-111111111111',
        '44444444-4444-4444-4444-444444444444',
        '22222222-2222-2222-2222-222222222222',
        'Sala 1')
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.servicio_recurso (tenant_id, servicio_id, recurso_id)
VALUES ('11111111-1111-1111-1111-111111111111',
        '33333333-3333-3333-3333-333333333333',
        '44444444-4444-4444-4444-444444444444')
ON CONFLICT DO NOTHING;

-- Lunes a viernes, 9 a 17.
INSERT INTO negocio.regla_disponibilidad
  (tenant_id, recurso_id, dia_semana, hora_inicio, hora_fin)
SELECT '11111111-1111-1111-1111-111111111111',
       '44444444-4444-4444-4444-444444444444',
       d, '09:00', '17:00'
FROM generate_series(1, 5) AS d
ON CONFLICT DO NOTHING;

INSERT INTO negocio.politica_version
  (tenant_id, id, servicio_id, version,
   rango_cancelacion_horas, rango_modificacion_horas, penalidad_pct)
VALUES ('11111111-1111-1111-1111-111111111111',
        '55555555-5555-5555-5555-555555555555',
        NULL, 1, 24, 12, 20.00)
ON CONFLICT (tenant_id, id) DO NOTHING;

-- Un segundo tenant, para poder demostrar que RLS lo esconde.
INSERT INTO plataforma.tenant (id, identificador, nombre, moneda, zona_horaria)
VALUES ('99999999-9999-9999-9999-999999999999',
        'otro-negocio', 'Otro Negocio', 'USD', 'America/New_York')
ON CONFLICT (id) DO NOTHING;

INSERT INTO negocio.sede (tenant_id, id, nombre, zona_horaria)
VALUES ('99999999-9999-9999-9999-999999999999',
        '88888888-8888-8888-8888-888888888888',
        'Sede Ajena', 'America/New_York')
ON CONFLICT (tenant_id, id) DO NOTHING;


-- -----------------------------------------------------------------------------
-- Un tercer tenant, SOLO para las pruebas automatizadas
-- -----------------------------------------------------------------------------
-- Es identico en forma al de demostracion --misma zona, misma moneda, mismo
-- horario de lunes a viernes de 09:00 a 17:00, mismo servicio de una hora a
-- 80.000-- para que las pruebas sigan pudiendo afirmar "ocho franjas" sin
-- depender de datos que alguien pueda tocar.
--
-- Existe por separado por una razon concreta: transicion_estado es append-only
-- por trigger (RF-28), asi que una reserva creada por el nucleo NO se puede
-- borrar despues sin desactivar la garantia que la propia prueba quiere que
-- siga en pie. Lo mas que puede hacer la limpieza es cancelarla, y eso
-- significa que cada pasada de `task ci` deja filas para siempre. Acumularlas
-- en el tenant de demostracion llenaba sus pantallas de basura de prueba; aqui
-- no le estorban a nadie.

INSERT INTO plataforma.tenant (id, identificador, nombre, moneda, zona_horaria)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'pruebas', 'Tenant de Pruebas', 'COP', 'America/Bogota')
ON CONFLICT (id) DO NOTHING;

INSERT INTO negocio.sede (tenant_id, id, nombre, zona_horaria, direccion)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
        'Sede de Pruebas', 'America/Bogota', 'Calle de Pruebas')
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.servicio
  (tenant_id, id, sede_id, nombre, duracion_min, precio_monto)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'cccccccc-cccc-cccc-cccc-cccccccccccc',
        'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
        'Sesion de una hora', 60, 80000.00)
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.recurso (tenant_id, id, sede_id, nombre)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'dddddddd-dddd-dddd-dddd-dddddddddddd',
        'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb',
        'Sala de Pruebas')
ON CONFLICT (tenant_id, id) DO NOTHING;

INSERT INTO negocio.servicio_recurso (tenant_id, servicio_id, recurso_id)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'cccccccc-cccc-cccc-cccc-cccccccccccc',
        'dddddddd-dddd-dddd-dddd-dddddddddddd')
ON CONFLICT DO NOTHING;

-- Lunes a viernes, 9 a 17: ocho franjas de una hora, que es lo que afirman las
-- pruebas de disponibilidad.
INSERT INTO negocio.regla_disponibilidad
  (tenant_id, recurso_id, dia_semana, hora_inicio, hora_fin)
SELECT 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
       'dddddddd-dddd-dddd-dddd-dddddddddddd',
       d, '09:00', '17:00'
FROM generate_series(1, 5) AS d
ON CONFLICT DO NOTHING;

INSERT INTO negocio.politica_version
  (tenant_id, id, servicio_id, version,
   rango_cancelacion_horas, rango_modificacion_horas, penalidad_pct)
VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
        'eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee',
        NULL, 1, 24, 12, 20.00)
ON CONFLICT (tenant_id, id) DO NOTHING;
