package plataforma

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metricas agrupa el registro de Prometheus del servicio y los instrumentos
// comunes a todos los binarios.
type Metricas struct {
	Registro *prometheus.Registry

	// PeticionesHTTP y DuracionHTTP son la base para vigilar RNF-01. La
	// duración se mide en histograma y no en media: un promedio de 80 ms puede
	// esconder que el percentil 99 está en 900 ms, y RNF-01 es un presupuesto
	// de cola, no de promedio.
	PeticionesHTTP *prometheus.CounterVec
	DuracionHTTP   *prometheus.HistogramVec
}

// NuevasMetricas crea un registro propio en vez de usar el global de
// prometheus. El global lleva colectores que se registran solos desde
// cualquier dependencia y acaba exponiendo métricas que nadie eligió.
func NuevasMetricas(cfg Config) *Metricas {
	registro := prometheus.NewRegistry()

	registro.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	// La etiqueta de servicio se añade una vez aquí y no en cada instrumento:
	// con 45 pods de siete binarios escribiendo a un mismo Prometheus
	// (ARQ-03), una serie sin origen no se puede atribuir.
	factoria := promauto.With(prometheus.WrapRegistererWith(
		prometheus.Labels{"servicio": cfg.Servicio}, registro,
	))

	return &Metricas{
		Registro: registro,
		PeticionesHTTP: factoria.NewCounterVec(prometheus.CounterOpts{
			Name: "http_peticiones_total",
			Help: "Peticiones HTTP atendidas, por ruta y código de estado.",
		}, []string{"metodo", "ruta", "estado"}),
		DuracionHTTP: factoria.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_duracion_segundos",
			Help: "Duración de las peticiones HTTP.",
			// Los cortes se agrupan alrededor de los 200 ms de RNF-01: sin un
			// corte justo en el presupuesto no se puede calcular qué fracción
			// de peticiones lo incumple.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.2, 0.3, 0.5, 1, 2.5},
		}, []string{"metodo", "ruta"}),
	}
}
