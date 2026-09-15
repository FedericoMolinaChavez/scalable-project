package pagos

import (
	"errors"
	"testing"
)

// El dinero es lo único de este paquete que se puede probar sin base ni red, y
// también lo que más caro sale equivocar: una conversión mal hecha no lanza
// ninguna excepción, cobra otro importe. De ahí que estas pruebas sean
// exhaustivas donde el resto del repositorio es representativo.

func TestUnImporteDeDosDecimalesVaEnLaUnidadMenor(t *testing.T) {
	casos := []struct {
		texto  string
		moneda string
		menor  int64
	}{
		{"80000.00", "COP", 8_000_000},
		{"80000.10", "COP", 8_000_010},
		{"0.01", "USD", 1},
		{"1", "EUR", 100},    // sin parte decimal en la columna
		{"1.5", "EUR", 150},  // un solo decimal
		{"1.50", "EUR", 150}, // el mismo, escrito completo
		{"999999.99", "USD", 99_999_999},
	}

	for _, caso := range casos {
		monto, err := DesdeTexto(caso.texto, caso.moneda)
		if err != nil {
			t.Fatalf("%s %s: %v", caso.texto, caso.moneda, err)
		}
		if monto.Menor() != caso.menor {
			t.Errorf("%s %s: se cobraría %d y debería ser %d",
				caso.texto, caso.moneda, monto.Menor(), caso.menor)
		}
	}
}

// La prueba que justifica la tabla de monedas.
//
// Para un yen, 100 significa cien yenes, no un yen. Multiplicar por 100 "porque
// el dinero tiene céntimos" cobraría cien veces de más, y es un error que no da
// ninguna señal hasta que alguien mira su extracto.
func TestUnaMonedaSinDecimalesNoSeMultiplicaPorCien(t *testing.T) {
	monto, err := DesdeTexto("8000", "JPY")
	if err != nil {
		t.Fatalf("8000 JPY: %v", err)
	}

	if monto.Menor() != 8000 {
		t.Fatalf("se cobrarían %d yenes en vez de 8000", monto.Menor())
	}
	if monto.Texto() != "8000" {
		t.Errorf("se mostraría %q en vez de \"8000\"", monto.Texto())
	}
}

func TestUnaMonedaDeTresDecimalesUsaMilesimas(t *testing.T) {
	monto, err := DesdeTexto("12.345", "KWD")
	if err != nil {
		t.Fatalf("12.345 KWD: %v", err)
	}

	if monto.Menor() != 12_345 {
		t.Fatalf("se cobrarían %d fils en vez de 12345", monto.Menor())
	}
}

// Truncar y no redondear, y hacia abajo.
//
// La columna es numeric(12,2) así que este caso no debería llegar nunca desde
// la base; llega desde un cálculo intermedio. Redondear hacia arriba cobraría
// más de lo que dice el precio congelado en la reserva, y ese es exactamente el
// lado por el que no se puede fallar.
func TestUnDecimalDeMasSeTruncaYNoSeRedondea(t *testing.T) {
	monto, err := DesdeTexto("10.999", "USD")
	if err != nil {
		t.Fatalf("10.999 USD: %v", err)
	}

	if monto.Menor() != 1099 {
		t.Fatalf("se cobrarían %d centavos en vez de 1099", monto.Menor())
	}
}

// Un importe que no se puede leer se rechaza en vez de interpretarse.
//
// La tentación es ser generoso —aceptar espacios, comas, signos— y es
// exactamente lo que no se puede hacer con dinero: un importe que se adivina es
// un importe que se cobra mal.
func TestUnImporteIlegibleSeRechaza(t *testing.T) {
	casos := []struct{ texto, moneda string }{
		{"", "COP"},
		{"-10.00", "COP"},    // un negativo no es un cobro
		{"80,000.00", "COP"}, // separador de miles
		{"1e5", "COP"},       // notación científica
		{"abc", "COP"},
		{"10.00", "CO"},   // moneda de dos letras
		{"10.00", "COPX"}, // de cuatro
	}

	for _, caso := range casos {
		if _, err := DesdeTexto(caso.texto, caso.moneda); !errors.Is(err, ErrMontoInvalido) {
			t.Errorf("%q %q: se aceptó (error %v) cuando debía rechazarse",
				caso.texto, caso.moneda, err)
		}
	}
}

// La ida y vuelta tiene que ser exacta: lo que sale hacia el contrato y hacia
// la columna se escribe con la escala de la moneda, no como se hubiera
// escrito en el origen.
func TestElTextoSeNormalizaALaEscalaDeLaMoneda(t *testing.T) {
	casos := []struct{ entrada, moneda, salida string }{
		{"80000", "COP", "80000.00"},
		{"80000.5", "COP", "80000.50"},
		{"8000", "JPY", "8000"},
		{"12.3", "KWD", "12.300"},
	}

	for _, caso := range casos {
		monto, err := DesdeTexto(caso.entrada, caso.moneda)
		if err != nil {
			t.Fatalf("%s %s: %v", caso.entrada, caso.moneda, err)
		}
		if monto.Texto() != caso.salida {
			t.Errorf("%s %s: se escribió %q y debía ser %q",
				caso.entrada, caso.moneda, monto.Texto(), caso.salida)
		}
	}
}

// El porcentaje lo usa el reembolso de RF-29 cuando la política congelada lleva
// penalidad. Trunca hacia abajo en la unidad menor, así que el redondeo se come
// como mucho un centavo y hacia el lado del cliente.
func TestElPorcentajeSeTruncaHaciaAbajo(t *testing.T) {
	monto, err := DesdeTexto("100.00", "USD")
	if err != nil {
		t.Fatal(err)
	}

	if parcial := monto.Porcentaje(33); parcial.Menor() != 3300 {
		t.Errorf("el 33%% de 100.00 dio %d centavos y debía dar 3300", parcial.Menor())
	}

	// Los extremos no se calculan, se devuelven: el 100% es el importe entero y
	// el 0% no es un reembolso.
	if todo := monto.Porcentaje(100); todo.Menor() != monto.Menor() {
		t.Errorf("el 100%% dio %d y debía dar %d", todo.Menor(), monto.Menor())
	}
	if nada := monto.Porcentaje(0); !nada.EsCero() {
		t.Errorf("el 0%% dio %d y debía dar 0", nada.Menor())
	}
}
