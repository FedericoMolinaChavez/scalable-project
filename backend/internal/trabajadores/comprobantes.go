package trabajadores

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/almacen"
	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/datos"
)

// El emisor de comprobantes de RF-34, y el único inquilino de MinIO en ARQ-01.
//
// Emite un comprobante por cada pago confirmado que todavía no lo tiene. No
// escucha eventos: barre. La diferencia importa porque un consumidor de NATS
// que pierde un mensaje pierde un comprobante para siempre, mientras que un
// barrido sobre "pagos confirmados sin comprobante" se cura solo en la
// siguiente pasada, venga de donde venga la confirmación —el webhook o el
// conciliador— y aunque el proceso muriera a mitad.
//
// Cada pasada hace dos cosas distintas, en este orden:
//
//	1. emitir las filas que faltan: consumir folio, congelar los datos
//	2. subir a MinIO los documentos de las filas que aún no lo tienen
//
// Están separadas porque el número tiene que existir ANTES de rendir el
// documento —va impreso dentro— y porque así una subida que falla no deshace la
// emisión: la fila queda con su número y sin objeto_clave, que es exactamente
// la condición por la que la siguiente pasada la vuelve a seleccionar. La ruta
// de lectura ya contempla esa ventana y devuelve el número sin enlace, tal como
// el contrato declara.
//
// Las dos van dentro de la misma transacción del tenant, y eso incluye la
// subida a MinIO. No es lo ideal —mantiene una transacción abierta durante una
// llamada de red— pero la alternativa es peor: `porCadaTenant` fija el tenant
// con SET LOCAL, así que fuera de la transacción no hay contexto con el que
// volver a leer estas filas bajo RLS, y habría que abrir una segunda. Lo que
// acota el coste es el tamaño del lote y que esto no está en ninguna ruta
// caliente: lo corre un trabajador sobre las filas de un solo tenant.

// Almacen es lo que este trabajador necesita del almacenamiento de objetos.
type Almacen interface {
	Guardar(ctx context.Context, clave string, contenido []byte, tipoMedio string) error
}

// Comprobantes construye el bucle.
func Comprobantes(
	bd *datos.BD, objetos Almacen, intervalo time.Duration, registro *slog.Logger,
) Bucle {
	return Bucle{
		Nombre:    "comprobantes",
		Intervalo: intervalo,
		Pasada: func(ctx context.Context) (int, error) {
			return porCadaTenant(ctx, bd, func(ctx context.Context, tx pgx.Tx, tenant string) (int, error) {
				return emitirLote(ctx, tx, tenant, objetos, registro)
			})
		},
	}
}

// datosComprobante es lo que se congela dentro del documento.
//
// Se guarda entero en la columna `datos` y no se relee de la reserva al
// mostrarlo. RF-34 dice que el comprobante se conserva aunque la cuenta se
// anonimice (RF-25), y un documento que se reconstruye cada vez que se mira no
// sobreviviría a eso: enseñaría los datos de hoy, no los de cuando se cobró.
type datosComprobante struct {
	Numero    string    `json:"numero"`
	EmitidoEn time.Time `json:"emitido_en"`

	Negocio string `json:"negocio"`
	Sede    string `json:"sede"`

	Servicio string    `json:"servicio"`
	Inicio   time.Time `json:"inicio"`
	Fin      time.Time `json:"fin"`
	Zona     string    `json:"zona_horaria"`

	Cliente string `json:"cliente"`
	Correo  string `json:"correo"`

	Monto  string `json:"monto"`
	Moneda string `json:"moneda"`

	// Referencia del proveedor, para poder cruzar este documento con el
	// extracto de Stripe sin abrir la base.
	Referencia string `json:"referencia"`
}

func emitirLote(
	ctx context.Context, tx pgx.Tx, tenant string, objetos Almacen, registro *slog.Logger,
) (int, error) {
	// Los pagos confirmados que todavía no tienen comprobante. El NOT EXISTS
	// sobre la restricción única es lo que hace este barrido idempotente: en
	// cuanto la fila existe, el pago deja de aparecer.
	filas, err := tx.Query(ctx, `
		SELECT p.id::text, p.reserva_id::text, p.payment_intent_id,
		       p.monto::text, p.moneda, p.confirmado_en,
		       t.nombre, sd.nombre, sd.zona_horaria,
		       s.nombre, lower(r.periodo), upper(r.periodo),
		       coalesce(r.contacto_nombre, ''), coalesce(r.contacto_email, '')
		FROM negocio.pago p
		JOIN negocio.reserva  r  ON r.tenant_id = p.tenant_id AND r.id = p.reserva_id
		JOIN negocio.servicio s  ON s.tenant_id = r.tenant_id AND s.id = r.servicio_id
		JOIN negocio.recurso  rc ON rc.tenant_id = r.tenant_id AND rc.id = r.recurso_id
		JOIN negocio.sede     sd ON sd.tenant_id = rc.tenant_id AND sd.id = rc.sede_id
		JOIN plataforma.tenant t ON t.id = p.tenant_id
		WHERE p.estado = 'confirmado'
		  AND NOT EXISTS (
		        SELECT 1 FROM negocio.comprobante c
		        WHERE c.tenant_id = p.tenant_id
		          AND c.reserva_id = p.reserva_id
		          AND c.tipo = 'pago')
		ORDER BY p.confirmado_en
		LIMIT $1
		FOR UPDATE OF p SKIP LOCKED`, LoteMaximo)
	if err != nil {
		return 0, err
	}

	type porEmitir struct {
		reservaID string
		datos     datosComprobante
	}

	var lote []porEmitir
	for filas.Next() {
		var (
			p           porEmitir
			pagoID      string
			confirmado  *time.Time
			negocio     string
			sede, zona  string
			servicio    string
			inicio, fin time.Time
		)
		if err := filas.Scan(&pagoID, &p.reservaID, &p.datos.Referencia,
			&p.datos.Monto, &p.datos.Moneda, &confirmado,
			&negocio, &sede, &zona,
			&servicio, &inicio, &fin,
			&p.datos.Cliente, &p.datos.Correo); err != nil {
			filas.Close()
			return 0, err
		}

		p.datos.Negocio, p.datos.Sede, p.datos.Zona = negocio, sede, zona
		p.datos.Servicio, p.datos.Inicio, p.datos.Fin = servicio, inicio, fin
		if confirmado != nil {
			p.datos.EmitidoEn = *confirmado
		}

		lote = append(lote, p)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	emitidos := 0
	for _, pendiente := range lote {
		numero, err := siguienteFolio(ctx, tx, tenant)
		if err != nil {
			return emitidos, err
		}
		pendiente.datos.Numero = numero
		pendiente.datos.EmitidoEn = time.Now()

		carga, err := json.Marshal(pendiente.datos)
		if err != nil {
			return emitidos, err
		}

		var comprobanteID string
		err = tx.QueryRow(ctx, `
			INSERT INTO negocio.comprobante (tenant_id, reserva_id, tipo, numero, datos)
			VALUES ($1::uuid, $2::uuid, 'pago', $3, $4::jsonb)
			RETURNING id::text`,
			tenant, pendiente.reservaID, numero, string(carga)).Scan(&comprobanteID)
		if err != nil {
			return emitidos, err
		}

		// Aquí termina la emisión. El documento lo sube el segundo recorrido,
		// que recoge esta fila junto con las que quedaron pendientes de pasadas
		// anteriores.
		emitidos++
		registro.DebugContext(ctx, "comprobante emitido",
			slog.String("numero", numero), slog.String("reserva", pendiente.reservaID))
	}

	// Y ahora los documentos que faltan por subir, incluidos los que acaban de
	// emitirse en esta misma pasada. Ver la nota de cabecera sobre por qué esto
	// ocurre dentro de la transacción.
	subidos, err := subirPendientes(ctx, tx, tenant, objetos, registro)
	if err != nil {
		return emitidos, err
	}

	// Se cuentan las dos cosas: emitir una fila y subir un documento son dos
	// unidades de trabajo distintas, y el bucle usa el total para decidir si
	// vuelve enseguida. Contar solo las emitidas dejaría una cola de documentos
	// pendientes drenando a un lote por intervalo.
	return emitidos + subidos, nil
}

// siguienteFolio consume un número del correlativo del tenant.
//
// UPDATE ... RETURNING sobre una fila: el bloqueo de fila serializa a los
// concurrentes, y el número se revierte con la transacción si algo falla
// después. Una SEQUENCE no haría ni lo uno ni lo otro —no se revierte, así que
// deja huecos— y un correlativo con huecos es justo lo que un número de
// comprobante existe para no tener.
//
// El INSERT ... ON CONFLICT crea la fila del año en curso la primera vez, sin
// que nadie tenga que acordarse de sembrarla cada 1 de enero.
func siguienteFolio(ctx context.Context, tx pgx.Tx, tenant string) (string, error) {
	anio := time.Now().Year()

	var siguiente int
	err := tx.QueryRow(ctx, `
		INSERT INTO negocio.folio_comprobante (tenant_id, anio, siguiente)
		VALUES ($1::uuid, $2, 2)
		ON CONFLICT (tenant_id, anio)
		DO UPDATE SET siguiente = negocio.folio_comprobante.siguiente + 1
		RETURNING siguiente - 1`,
		tenant, anio).Scan(&siguiente)
	if err != nil {
		return "", fmt.Errorf("no se pudo consumir el folio de %d: %w", anio, err)
	}

	// Seis dígitos con ceros a la izquierda: ordena igual como texto que como
	// número, que es lo que permite paginar y comparar sin convertir.
	return fmt.Sprintf("%d-%06d", anio, siguiente), nil
}

// subirPendientes sube los documentos de los comprobantes sin objeto_clave.
func subirPendientes(
	ctx context.Context, tx pgx.Tx, tenant string, objetos Almacen, registro *slog.Logger,
) (int, error) {
	if objetos == nil {
		// Sin almacén se emiten igual, solo que sin documento. Es una
		// degradación honesta: el comprobante existe con su número y la ruta de
		// lectura ya sabe devolverlo sin enlace.
		return 0, nil
	}

	filas, err := tx.Query(ctx, `
		SELECT id::text, datos::text
		FROM negocio.comprobante
		WHERE objeto_clave IS NULL
		ORDER BY emitido_en
		LIMIT $1
		FOR UPDATE SKIP LOCKED`, LoteMaximo)
	if err != nil {
		return 0, err
	}

	type porSubir struct{ id, datos string }
	var lote []porSubir
	for filas.Next() {
		var p porSubir
		if err := filas.Scan(&p.id, &p.datos); err != nil {
			filas.Close()
			return 0, err
		}
		lote = append(lote, p)
	}
	filas.Close()
	if err := filas.Err(); err != nil {
		return 0, err
	}

	subidos := 0
	for _, pendiente := range lote {
		var datos datosComprobante
		if err := json.Unmarshal([]byte(pendiente.datos), &datos); err != nil {
			registro.ErrorContext(ctx, "comprobante con datos ilegibles; no se puede rendir",
				slog.String("comprobante", pendiente.id), slog.String("error", err.Error()))
			continue
		}

		documento, err := rendir(datos)
		if err != nil {
			return subidos, err
		}

		clave := almacen.Clave(tenant, pendiente.id)
		if err := objetos.Guardar(ctx, clave, documento, "text/html; charset=utf-8"); err != nil {
			// El almacén falló. Se corta y se reintenta en la siguiente pasada:
			// la fila sigue sin objeto_clave, que es exactamente la condición
			// que la vuelve a seleccionar.
			registro.WarnContext(ctx, "no se pudo subir el comprobante; se reintentará",
				slog.String("comprobante", pendiente.id), slog.String("error", err.Error()))
			break
		}

		if _, err := tx.Exec(ctx,
			`UPDATE negocio.comprobante SET objeto_clave = $2 WHERE id = $1`,
			pendiente.id, clave); err != nil {
			return subidos, err
		}

		subidos++
	}

	return subidos, nil
}

// rendir produce el documento.
//
// Es HTML y no PDF, y conviene decir por qué antes de que parezca un atajo: un
// PDF exige una biblioteca de composición o un navegador headless, y ninguna de
// las dos aporta nada a lo que RF-34 pide —un comprobante que se pueda guardar
// e imprimir—. El HTML se imprime a PDF desde cualquier navegador con la hoja
// de estilos de impresión que lleva dentro, se archiva igual y se lee sin
// programa aparte. El día que haga falta un formato con firma electrónica, esto
// cambia aquí y en ningún otro sitio.
//
// html/template y no text/template: los datos del comprobante incluyen el
// nombre que una persona escribió en un formulario. Con text/template, un
// nombre con `<script>` dentro se convertiría en script ejecutable dentro de un
// documento que otra persona abre.
func rendir(datos datosComprobante) ([]byte, error) {
	zona, err := time.LoadLocation(datos.Zona)
	if err != nil {
		// La zona del tenant no se reconoce en esta máquina. Se cae a UTC y se
		// dice en el documento, en vez de imprimir una hora que parece local y
		// no lo es.
		zona = time.UTC
	}

	var salida strings.Builder
	err = plantillaComprobante.Execute(&salida, struct {
		datosComprobante
		InicioLocal string
		FinLocal    string
		EmitidoLoc  string
	}{
		datosComprobante: datos,
		InicioLocal:      datos.Inicio.In(zona).Format("02/01/2006 15:04"),
		FinLocal:         datos.Fin.In(zona).Format("15:04"),
		EmitidoLoc:       datos.EmitidoEn.In(zona).Format("02/01/2006 15:04"),
	})
	if err != nil {
		return nil, fmt.Errorf("no se pudo rendir el comprobante %s: %w", datos.Numero, err)
	}

	return []byte(salida.String()), nil
}

var plantillaComprobante = template.Must(template.New("comprobante").Parse(`<!doctype html>
<html lang="es">
<head>
<meta charset="utf-8">
<title>Comprobante {{.Numero}}</title>
<style>
  :root { color-scheme: light; }
  body { font: 15px/1.6 ui-sans-serif, system-ui, sans-serif; color: #18181b;
         max-width: 44rem; margin: 3rem auto; padding: 0 1.5rem; background: #fff; }
  h1 { font-size: 1.05rem; letter-spacing: .08em; text-transform: uppercase;
       color: #71717a; margin: 0 0 .25rem; }
  .numero { font-size: 2rem; font-weight: 600; letter-spacing: -.02em; margin: 0 0 2rem; }
  dl { display: grid; grid-template-columns: 11rem 1fr; gap: .6rem 1.5rem; margin: 0 0 2rem; }
  dt { color: #71717a; }
  dd { margin: 0; }
  .total { border-top: 1px solid #e4e4e7; padding-top: 1.25rem;
           display: flex; justify-content: space-between; align-items: baseline;
           font-size: 1.35rem; font-weight: 600; }
  footer { margin-top: 3rem; color: #a1a1aa; font-size: .8125rem; }
  @media print { body { margin: 0; } }
</style>
</head>
<body>
  <h1>Comprobante de pago</h1>
  <p class="numero">{{.Numero}}</p>

  <dl>
    <dt>Negocio</dt><dd>{{.Negocio}}</dd>
    <dt>Sede</dt><dd>{{.Sede}}</dd>
    <dt>Servicio</dt><dd>{{.Servicio}}</dd>
    <dt>Horario</dt><dd>{{.InicioLocal}} – {{.FinLocal}} ({{.Zona}})</dd>
    <dt>A nombre de</dt><dd>{{.Cliente}}</dd>
    <dt>Correo</dt><dd>{{.Correo}}</dd>
    <dt>Emitido</dt><dd>{{.EmitidoLoc}}</dd>
    <dt>Referencia</dt><dd>{{.Referencia}}</dd>
  </dl>

  <div class="total">
    <span>Total pagado</span>
    <span>{{.Monto}} {{.Moneda}}</span>
  </div>

  <footer>
    El horario indicado corresponde a la zona de la sede. El instante final no
    forma parte de la reserva: la cita siguiente puede empezar a esa hora.
  </footer>
</body>
</html>
`))
