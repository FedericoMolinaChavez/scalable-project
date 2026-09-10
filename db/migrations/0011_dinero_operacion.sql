-- =============================================================================
-- 0011 - Dinero y operacion (ER-03)
-- =============================================================================
-- La mitad de ER-03 que faltaba: lo que ocurre con el dinero despues de que el
-- nucleo garantice el cupo, y el rollup que responde RF-11 sin motor analitico
-- aparte.
--
-- Todo lo de aqui vive FUERA de la transaccion del nucleo, y esa es la razon de
-- que sean tablas y no columnas de reserva. La transaccion que sostiene la
-- invariante de RNF-10 no puede esperar a una llamada a Stripe: ARQ-01 saca a
-- los trabajadores asincronos "todo lo que tolera latencia o depende de
-- terceros", y el pago es el caso original de esa regla.
--
-- La idempotencia se repite en tres niveles distintos, porque los tres reciben
-- entregas at-least-once desde sistemas que no controlamos:
--
--   plataforma.evento_webhook.evento_id       no procesar dos veces un evento
--   negocio.reembolso  (tenant_id, pago_id)   no devolver dos veces un pago
--   negocio.comprobante (tenant_id, reserva_id, tipo)  no emitir dos veces
-- =============================================================================


-- -----------------------------------------------------------------------------
-- Tipos
-- -----------------------------------------------------------------------------
-- proveedor_pago vive en plataforma y no en negocio porque la integracion con
-- Stripe es de la CUENTA de la plataforma, no de cada negocio: es el mismo enum
-- que necesita plataforma.evento_webhook, que es global. Los demas son de
-- negocio, porque describen filas que si llevan tenant_id.

CREATE TYPE plataforma.proveedor_pago AS ENUM (
  'stripe'
);

CREATE TYPE negocio.estado_pago AS ENUM (
  'iniciado',     -- hay PaymentIntent; el dinero todavia no se movio
  'confirmado',   -- el webhook de RF-33 lo dio por cobrado
  'fallido'       -- la tarjeta lo rechazo, o el intento se cancelo
);

CREATE TYPE negocio.motivo_reembolso AS ENUM (
  'cancelacion_usuario',
  'cancelacion_negocio',
  'sin_cupo'      -- se cobro por una reserva que ya no existia (RF-33)
);

CREATE TYPE negocio.estado_reembolso AS ENUM (
  'pendiente',
  'confirmado',
  'fallido'
);

CREATE TYPE negocio.tipo_comprobante AS ENUM (
  'pago',
  'nota_credito'
);


-- -----------------------------------------------------------------------------
-- pago
-- -----------------------------------------------------------------------------
-- Nunca guarda datos de tarjeta (RNF-05). Lo unico que se conserva es el
-- identificador tokenizado que Stripe emite, que por si solo no permite cobrar
-- nada: el numero de la tarjeta no llega a este backend en ningun momento,
-- porque el navegador habla directamente con Stripe usando el client_secret.

CREATE TABLE negocio.pago (
  tenant_id         uuid NOT NULL,
  id                uuid NOT NULL DEFAULT gen_random_uuid(),

  reserva_id        uuid NOT NULL,

  proveedor         plataforma.proveedor_pago NOT NULL DEFAULT 'stripe',
  payment_intent_id text NOT NULL,

  -- Mismo snapshot financiero que la reserva, y por la misma razon (RF-31,
  -- RF-34): un comprobante ya emitido debe poder reproducirse aunque el negocio
  -- cambie de precio o de moneda despues.
  monto             numeric(12,2) NOT NULL,
  moneda            char(3) NOT NULL,

  estado            negocio.estado_pago NOT NULL DEFAULT 'iniciado',

  -- Lo que dijo el proveedor cuando fallo. Se guarda porque es lo unico que
  -- permite responder "por que no se pudo cobrar" sin entrar al panel de
  -- Stripe, y porque el motivo decide si tiene sentido reintentar.
  motivo_fallo      text,

  creado_en         timestamptz NOT NULL DEFAULT now(),
  confirmado_en     timestamptz,

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, reserva_id)
    REFERENCES negocio.reserva (tenant_id, id),

  -- ER-03 pide payment_intent_id UNIQUE a secas. En una tabla particionada por
  -- HASH(tenant_id) un indice unico DEBE incluir la clave de particion, asi que
  -- la unicidad es por tenant. No se pierde nada: el identificador lo genera
  -- Stripe y ya es unico en el universo, de modo que dos tenants no pueden
  -- colisionar ni queriendo. Lo que si obliga es a que el webhook sepa a que
  -- tenant pertenece el evento antes de buscarlo, y para eso el tenant viaja en
  -- la metadata del PaymentIntent (ver internal/pagos).
  CONSTRAINT pago_intento_uq UNIQUE (tenant_id, payment_intent_id),

  CONSTRAINT pago_monto_no_negativo CHECK (monto >= 0),
  CONSTRAINT pago_moneda_iso        CHECK (moneda ~ '^[A-Z]{3}$'),

  -- Un pago confirmado sin fecha de confirmacion no se puede conciliar contra
  -- el extracto del proveedor, y un pago no confirmado con ella es una
  -- contradiccion. El motor lo impide en vez de confiar en el codigo.
  CONSTRAINT pago_confirmado_coherente CHECK (
    (estado = 'confirmado') = (confirmado_en IS NOT NULL)
  )
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'pago');

COMMENT ON TABLE negocio.pago IS
  'Intento de cobro contra el proveedor. Nunca almacena datos de tarjeta '
  '(RNF-05): solo el identificador tokenizado del PaymentIntent.';

COMMENT ON COLUMN negocio.pago.payment_intent_id IS
  'Identificador de Stripe. Es tambien la clave con la que el webhook de RF-33 '
  'encuentra esta fila, y la que hace idempotente reintentar la intencion: '
  'pedirla dos veces para la misma reserva devuelve el mismo PaymentIntent.';


-- Un pago iniciado por reserva, no dos.
--
-- Sin esto, dos pestanas abiertas sobre la misma reserva crearian dos
-- PaymentIntent y la persona podria pagar los dos: la restriccion EXCLUDE
-- protege el cupo, pero nada protegeria el dinero. Es parcial porque una
-- reserva SI puede acumular varios pagos a lo largo del tiempo --uno fallido y
-- otro que si cobra-- y lo que no puede tener es dos vivos a la vez.
CREATE UNIQUE INDEX pago_un_intento_vivo_por_reserva
  ON negocio.pago (tenant_id, reserva_id)
  WHERE estado = 'iniciado';

-- El conciliador de RF-33 pregunta siempre lo mismo: que pagos llevan demasiado
-- tiempo iniciados sin que llegara su webhook.
CREATE INDEX pago_iniciados_por_antiguedad
  ON negocio.pago (creado_en)
  WHERE estado = 'iniciado';


-- -----------------------------------------------------------------------------
-- plataforma.evento_webhook
-- -----------------------------------------------------------------------------
-- GLOBAL, no por tenant, y no es un descuido de ER-03: los eventos de Stripe
-- son de la cuenta de la plataforma. Cuando uno llega todavia no se sabe de que
-- negocio es --eso se descubre leyendo su metadata-- asi que una tabla con
-- tenant_id NOT NULL no podria ni registrar la llegada del evento que no se
-- pudo asociar a nadie, que es justamente el caso que hay que poder auditar.
--
-- Por eso vive en plataforma, que es el esquema sin RLS por tenant.

CREATE TABLE plataforma.evento_webhook (
  id           uuid NOT NULL DEFAULT gen_random_uuid() PRIMARY KEY,

  proveedor    plataforma.proveedor_pago NOT NULL DEFAULT 'stripe',

  -- La idempotencia de RF-33. Stripe reintenta durante dias ante cualquier
  -- respuesta que no sea 2xx, asi que recibir el mismo evento varias veces es
  -- lo normal, no la excepcion.
  evento_id    text NOT NULL,
  tipo         text NOT NULL,
  payload      jsonb,

  recibido_en  timestamptz NOT NULL DEFAULT now(),

  -- NULL mientras no se haya aplicado el efecto de negocio. Distinguir
  -- "recibido" de "procesado" es lo que permite escribir la fila nada mas
  -- validar la firma: si el proceso muere a mitad del efecto, el evento consta
  -- como recibido y sin procesar, y se puede reintentar sabiendo cual era.
  procesado_en timestamptz,

  -- Por que no se pudo procesar, cuando aplica. Un evento que falla en silencio
  -- es dinero que se movio sin que la reserva se entere.
  error        text,

  CONSTRAINT evento_webhook_uq UNIQUE (proveedor, evento_id)
);

COMMENT ON TABLE plataforma.evento_webhook IS
  'Bitacora de los webhooks del proveedor de pago. UNIQUE (proveedor, '
  'evento_id) es la idempotencia de RF-33 ante entrega at-least-once.';

CREATE INDEX evento_webhook_sin_procesar
  ON plataforma.evento_webhook (recibido_en)
  WHERE procesado_en IS NULL;


-- -----------------------------------------------------------------------------
-- reembolso
-- -----------------------------------------------------------------------------
-- RF-29. Una fila por pago, y ese UNIQUE es la idempotencia: el trabajador que
-- devuelve el dinero puede morir entre llamar a Stripe y marcar la fila, y sin
-- una clave que lo impida la siguiente pasada reembolsaria otra vez.

CREATE TABLE negocio.reembolso (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL DEFAULT gen_random_uuid(),

  pago_id      uuid NOT NULL,

  -- No tiene por que ser el importe completo: la politica congelada en la
  -- reserva puede llevar penalidad_pct (RF-15), y entonces se devuelve solo la
  -- parte que corresponda.
  monto        numeric(12,2) NOT NULL,
  motivo       negocio.motivo_reembolso NOT NULL,
  estado       negocio.estado_reembolso NOT NULL DEFAULT 'pendiente',

  -- El identificador del refund en Stripe, cuando ya existe. Es lo que permite
  -- que un reintento pregunte por el estado en vez de crear otro.
  refund_id    text,

  intentos     int NOT NULL DEFAULT 0,
  ultimo_error text,

  creado_en    timestamptz NOT NULL DEFAULT now(),
  resuelto_en  timestamptz,

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, pago_id)
    REFERENCES negocio.pago (tenant_id, id),

  CONSTRAINT reembolso_uno_por_pago UNIQUE (tenant_id, pago_id),

  CONSTRAINT reembolso_monto_positivo        CHECK (monto > 0),
  CONSTRAINT reembolso_intentos_no_negativos CHECK (intentos >= 0)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'reembolso');

COMMENT ON TABLE negocio.reembolso IS
  'RF-29. UNIQUE (tenant_id, pago_id) es la idempotencia: el trabajador puede '
  'morir entre llamar al proveedor y marcar la fila, y esta clave impide que '
  'la siguiente pasada devuelva el dinero otra vez.';

CREATE INDEX reembolso_pendientes
  ON negocio.reembolso (creado_en)
  WHERE estado = 'pendiente';


-- -----------------------------------------------------------------------------
-- comprobante
-- -----------------------------------------------------------------------------
-- RF-34. Se conserva aunque la cuenta se anonimice (RF-25), asi que los datos
-- que lo componen se congelan en `datos` en vez de leerse de la reserva al
-- mirarlo: un comprobante que se reconstruye cada vez que se consulta no es un
-- comprobante, es una consulta.

CREATE TABLE negocio.comprobante (
  tenant_id    uuid NOT NULL,
  id           uuid NOT NULL DEFAULT gen_random_uuid(),

  reserva_id   uuid NOT NULL,
  tipo         negocio.tipo_comprobante NOT NULL,
  numero       text NOT NULL,
  datos        jsonb NOT NULL,

  -- AGREGADO respecto a ER-03. El modelo describe el comprobante como dato y no
  -- dice donde vive el documento; ARQ-01 si lo dice --MinIO-- y sin esta
  -- columna no habria forma de volver a encontrarlo. Es la clave del objeto, no
  -- una URL: las URL firmadas caducan y llevan la firma dentro, guardarlas
  -- seria guardar una credencial con fecha.
  objeto_clave text,

  emitido_en   timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, reserva_id)
    REFERENCES negocio.reserva (tenant_id, id),

  CONSTRAINT comprobante_numero_uq UNIQUE (tenant_id, numero),

  -- La idempotencia del emisor. Sin ella, dos entregas del mismo webhook emiten
  -- dos comprobantes del mismo cobro con numeros distintos, que es exactamente
  -- el error que un numero correlativo existe para hacer visible.
  CONSTRAINT comprobante_uno_por_reserva_y_tipo UNIQUE (tenant_id, reserva_id, tipo)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'comprobante');

COMMENT ON TABLE negocio.comprobante IS
  'RF-34. Se conserva aunque la cuenta se anonimice (RF-25), y por eso congela '
  'sus datos en lugar de releerlos de la reserva.';


-- folio: el correlativo por tenant y por ano.
--
-- Una SEQUENCE de PostgreSQL no sirve aqui, y conviene decir por que antes de
-- que alguien lo "simplifique": las secuencias no se revierten con la
-- transaccion, asi que dejan huecos, y un comprobante numerado con huecos es
-- justo lo que un correlativo no puede tener. Ademas harian falta una secuencia
-- por tenant y por ano, creadas dinamicamente.
--
-- Un contador en una fila si se revierte con la transaccion que lo consumio, y
-- el UPDATE ... RETURNING serializa a los concurrentes por bloqueo de fila. El
-- coste es que los comprobantes de un mismo tenant se emiten de uno en uno, y
-- eso es aceptable: no estan en la ruta de los 200 ms de RNF-01, los emite un
-- trabajador asincrono.
CREATE TABLE negocio.folio_comprobante (
  tenant_id uuid NOT NULL,
  anio      int  NOT NULL,
  siguiente int  NOT NULL DEFAULT 1,

  PRIMARY KEY (tenant_id, anio),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),

  CONSTRAINT folio_siguiente_positivo CHECK (siguiente >= 1)
) PARTITION BY HASH (tenant_id);

CALL infra.particionar_hash('negocio', 'folio_comprobante');

COMMENT ON TABLE negocio.folio_comprobante IS
  'Correlativo de comprobantes por tenant y ano. No es una SEQUENCE a '
  'proposito: las secuencias no se revierten y dejan huecos.';


-- -----------------------------------------------------------------------------
-- metrica_diaria
-- -----------------------------------------------------------------------------
-- El rollup que resuelve RF-11 sin motor analitico aparte. Los indicadores del
-- requisito son agregados sobre dimensiones conocidas --fecha, sede, servicio--
-- y no busqueda ad-hoc, asi que precalcularlos en PostgreSQL y servirlos desde
-- las replicas de lectura cubre el caso entero.
--
-- No va particionada por HASH como el resto de negocio.*: es la unica tabla del
-- esquema cuyo tamano lo acota el calendario y no el trafico. Un tenant con
-- diez sedes y veinte servicios genera 200 filas al dia; 24 meses de historia
-- son 150.000 filas, que caben en un indice sin despeinarse.

CREATE TABLE negocio.metrica_diaria (
  tenant_id            uuid NOT NULL,
  fecha                date NOT NULL,
  sede_id              uuid NOT NULL,
  servicio_id          uuid NOT NULL,

  reservas_creadas     int NOT NULL DEFAULT 0,
  reservas_confirmadas int NOT NULL DEFAULT 0,
  reservas_canceladas  int NOT NULL DEFAULT 0,
  reservas_completadas int NOT NULL DEFAULT 0,
  reservas_no_show     int NOT NULL DEFAULT 0,

  ingresos             numeric(14,2) NOT NULL DEFAULT 0,
  minutos_ocupados     int NOT NULL DEFAULT 0,

  actualizada_en       timestamptz NOT NULL DEFAULT now(),

  PRIMARY KEY (tenant_id, fecha, sede_id, servicio_id),

  FOREIGN KEY (tenant_id) REFERENCES plataforma.tenant (id),
  FOREIGN KEY (tenant_id, sede_id)     REFERENCES negocio.sede (tenant_id, id),
  FOREIGN KEY (tenant_id, servicio_id) REFERENCES negocio.servicio (tenant_id, id),

  CONSTRAINT metrica_conteos_no_negativos CHECK (
    reservas_creadas >= 0 AND reservas_confirmadas >= 0
    AND reservas_canceladas >= 0 AND reservas_completadas >= 0
    AND reservas_no_show >= 0 AND minutos_ocupados >= 0
  )
);

COMMENT ON TABLE negocio.metrica_diaria IS
  'Rollup precalculado de RF-11. Se reconstruye entero desde las tablas base, '
  'asi que una pasada perdida o repetida del trabajador no lo corrompe.';

-- La consulta del panel es "un tenant, un rango de fechas", y la clave primaria
-- ya empieza por (tenant_id, fecha). No hace falta ningun indice mas.


-- -----------------------------------------------------------------------------
-- RLS y privilegios
-- -----------------------------------------------------------------------------
-- La migracion 0007 recorrio las tablas que existian ENTONCES. Estas nacen sin
-- RLS y sin privilegios, y si nadie lo hace aqui se quedan asi: es la clase de
-- omision que no da ningun error, solo deja de aislar.

DO $$
DECLARE
  tabla text;
BEGIN
  FOREACH tabla IN ARRAY ARRAY[
    'pago', 'reembolso', 'comprobante', 'folio_comprobante', 'metrica_diaria'
  ] LOOP
    EXECUTE format('ALTER TABLE negocio.%I ENABLE ROW LEVEL SECURITY', tabla);
    EXECUTE format('ALTER TABLE negocio.%I FORCE  ROW LEVEL SECURITY', tabla);

    EXECUTE format(
      'CREATE POLICY aislamiento_tenant ON negocio.%I '
      'FOR ALL TO reservas_app, reservas_lectura '
      'USING (tenant_id = infra.tenant_actual()) '
      'WITH CHECK (tenant_id = infra.tenant_actual())',
      tabla);

    EXECUTE format(
      'GRANT SELECT, INSERT, UPDATE ON negocio.%I TO reservas_app', tabla);
    EXECUTE format(
      'GRANT SELECT ON negocio.%I TO reservas_lectura', tabla);
  END LOOP;
END;
$$;

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
    JOIN pg_inherits i  ON i.inhrelid = c.oid
    JOIN pg_class p     ON p.oid = i.inhparent
    JOIN pg_namespace n ON n.oid = p.relnamespace
    WHERE n.nspname = 'negocio'
      AND p.relname IN ('pago', 'reembolso', 'comprobante', 'folio_comprobante')
  LOOP
    EXECUTE format(
      'REVOKE ALL ON negocio.%I FROM reservas_app, reservas_lectura',
      particion.relname);
  END LOOP;
END;
$$;

-- Un comprobante emitido no se corrige, se anula con una nota de credito
-- (RF-34). Igual que politica_version y transicion_estado: el REVOKE es la capa
-- que actua en produccion y el disparador la que evita que una correccion
-- manual reescriba historia.
REVOKE UPDATE, DELETE ON negocio.comprobante FROM reservas_app;

-- Salvo objeto_clave. El documento se sube a MinIO DESPUES de emitir la fila
-- --el numero tiene que existir para poder imprimirlo dentro-- asi que esa
-- columna se escribe una vez, ya con el objeto arriba. El resto sigue siendo
-- inmutable.
GRANT UPDATE (objeto_clave) ON negocio.comprobante TO reservas_app;

CREATE TRIGGER comprobante_append_only
  BEFORE DELETE ON negocio.comprobante
  FOR EACH STATEMENT EXECUTE FUNCTION infra.prohibir_mutacion();

-- plataforma.evento_webhook no lleva RLS por tenant porque no tiene tenant. Su
-- aislamiento es de privilegios: solo la aplicacion escribe en ella.
GRANT SELECT, INSERT, UPDATE ON plataforma.evento_webhook TO reservas_app;
GRANT SELECT                 ON plataforma.evento_webhook TO reservas_lectura;

GRANT ALL ON ALL TABLES IN SCHEMA plataforma, negocio TO reservas_soporte;
