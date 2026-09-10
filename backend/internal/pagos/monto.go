package pagos

import (
	"errors"
	"fmt"
	"strings"
)

// El dinero, entre PostgreSQL y Stripe.
//
// Los dos lados son exactos y ninguno usa coma flotante, pero no hablan igual:
// la columna es `numeric(12,2)` y viaja como texto ("80000.00"), y Stripe cobra
// en la unidad MENOR de la moneda, como entero. Convertir mal aquí no produce
// un error, produce un cobro por el importe equivocado, que es la peor clase de
// fallo silencioso que este sistema puede tener.
//
// Por eso la conversión vive en un tipo propio, con su prueba, y no en un
// `strconv` suelto dentro del manejador.

// ErrMontoInvalido: el texto no es un decimal que se pueda cobrar.
var ErrMontoInvalido = errors.New("el importe no tiene un formato que se pueda cobrar")

// Monto es un importe exacto en una moneda concreta.
type Monto struct {
	// Unidades y centesimas se guardan por separado en vez de como un float:
	// 80000.10 no se representa exacto en binario, y un céntimo de diferencia
	// entre lo que se muestra y lo que se cobra es una incidencia de soporte.
	unidades  int64
	decimales int64 // en la unidad menor, ya escalado

	Moneda string
}

// monedasSinDecimales son las que Stripe cobra sin subdivisión.
//
// La lista no es una curiosidad: para un yen, 100 significa 100 yenes, no un
// yen. Multiplicar por 100 "porque el dinero tiene céntimos" cobraría cien
// veces de más, y es un error que no da ninguna señal hasta que alguien mira su
// extracto.
var monedasSinDecimales = map[string]bool{
	"BIF": true, "CLP": true, "DJF": true, "GNF": true, "JPY": true,
	"KMF": true, "KRW": true, "MGA": true, "PYG": true, "RWF": true,
	"UGX": true, "VND": true, "VUV": true, "XAF": true, "XOF": true,
	"XPF": true,
}

// monedasTresDecimales llevan milésimas. El caso raro que igualmente rompe: un
// dinar kuwaití se subdivide en mil fils, no en cien.
var monedasTresDecimales = map[string]bool{
	"BHD": true, "JOD": true, "KWD": true, "OMR": true, "TND": true,
}

// decimalesDe dice en cuántas partes se subdivide la unidad de una moneda.
func decimalesDe(moneda string) int {
	switch {
	case monedasSinDecimales[moneda]:
		return 0
	case monedasTresDecimales[moneda]:
		return 3
	default:
		return 2
	}
}

// DesdeTexto convierte lo que viene de la columna numeric.
//
// Acepta "80000", "80000.5" y "80000.50", que es lo que PostgreSQL puede
// devolver según el tipo con el que se lea, y rechaza cualquier otra cosa en
// vez de interpretarla generosamente: un importe que se adivina es un importe
// que se cobra mal.
func DesdeTexto(texto, moneda string) (Monto, error) {
	moneda = strings.ToUpper(strings.TrimSpace(moneda))
	if len(moneda) != 3 {
		return Monto{}, fmt.Errorf("%w: moneda %q", ErrMontoInvalido, moneda)
	}

	texto = strings.TrimSpace(texto)
	if texto == "" || strings.HasPrefix(texto, "-") {
		// Un importe negativo no es un cobro. Si aparece, hay un error aguas
		// arriba y lo que toca es pararse, no cobrar su valor absoluto.
		return Monto{}, fmt.Errorf("%w: %q", ErrMontoInvalido, texto)
	}

	entera, fraccion, _ := strings.Cut(texto, ".")

	unidades, err := aEntero(entera)
	if err != nil {
		return Monto{}, fmt.Errorf("%w: %q", ErrMontoInvalido, texto)
	}

	escala := decimalesDe(moneda)

	// La fracción se ajusta a la escala de la moneda por relleno o por corte.
	// El corte es truncamiento y no redondeo, y es la decisión conservadora:
	// redondear hacia arriba cobraría más de lo que dice el precio congelado en
	// la reserva.
	if len(fraccion) > escala {
		fraccion = fraccion[:escala]
	}
	for len(fraccion) < escala {
		fraccion += "0"
	}

	var decimales int64
	if fraccion != "" {
		if decimales, err = aEntero(fraccion); err != nil {
			return Monto{}, fmt.Errorf("%w: %q", ErrMontoInvalido, texto)
		}
	}

	return Monto{unidades: unidades, decimales: decimales, Moneda: moneda}, nil
}

// Menor devuelve el importe en la unidad menor de la moneda, que es como cobra
// Stripe: 80000.00 COP son 8000000 centavos, y 8000 JPY son 8000 yenes.
func (m Monto) Menor() int64 {
	factor := int64(1)
	for range decimalesDe(m.Moneda) {
		factor *= 10
	}
	return m.unidades*factor + m.decimales
}

// Texto devuelve el importe tal como lo espera el contrato y la columna: una
// cadena decimal con la escala de la moneda.
func (m Monto) Texto() string {
	escala := decimalesDe(m.Moneda)
	if escala == 0 {
		return fmt.Sprintf("%d", m.unidades)
	}
	return fmt.Sprintf("%d.%0*d", m.unidades, escala, m.decimales)
}

// Porcentaje devuelve la parte proporcional, truncada hacia abajo.
//
// La usa el reembolso de RF-29 cuando la política congelada lleva penalidad. Se
// trunca en la unidad menor y no en la mayor, así que el redondeo se come como
// mucho un centavo, y hacia el lado del cliente: se devuelve de menos antes que
// de más solo por una división.
func (m Monto) Porcentaje(pct int) Monto {
	if pct <= 0 {
		return Monto{Moneda: m.Moneda}
	}
	if pct >= 100 {
		return m
	}

	menor := m.Menor() * int64(pct) / 100
	return DesdeMenor(menor, m.Moneda)
}

// DesdeMenor reconstruye un Monto desde la unidad menor.
func DesdeMenor(menor int64, moneda string) Monto {
	moneda = strings.ToUpper(moneda)

	factor := int64(1)
	for range decimalesDe(moneda) {
		factor *= 10
	}

	return Monto{
		unidades:  menor / factor,
		decimales: menor % factor,
		Moneda:    moneda,
	}
}

// EsCero dice si no hay nada que cobrar ni que devolver.
func (m Monto) EsCero() bool { return m.Menor() == 0 }

// aEntero convierte sin admitir signos, espacios ni notación científica.
// strconv.ParseInt aceptaría "+5" y " 5", y aquí eso solo puede venir de un
// dato que ya está mal.
func aEntero(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}

	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%w: %q no es un número", ErrMontoInvalido, s)
		}
		n = n*10 + int64(r-'0')
	}
	return n, nil
}
