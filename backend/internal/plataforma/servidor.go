package plataforma

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Comprobacion es una dependencia cuya salud decide si el servicio puede
// atender tráfico.
type Comprobacion struct {
	Nombre    string
	Verificar func(context.Context) error
}

// Servidor es el HTTP común a todos los binarios: expone las sondas y las
// métricas, monta las rutas del servicio y se apaga sin cortar peticiones a
// medias.
type Servidor struct {
	cfg            Config
	registro       *slog.Logger
	metricas       *Metricas
	comprobaciones []Comprobacion
	rutas          *http.ServeMux
}

// NuevoServidor prepara el servidor con /salud, /listo y /metrics ya montadas.
func NuevoServidor(cfg Config, registro *slog.Logger, metricas *Metricas) *Servidor {
	s := &Servidor{
		cfg:      cfg,
		registro: registro,
		metricas: metricas,
		rutas:    http.NewServeMux(),
	}

	// Vivo y listo son sondas distintas y confundirlas cuesta caro en
	// Kubernetes. Vivo dice "el proceso no está colgado"; si falla, el pod se
	// reinicia. Listo dice "puedo atender"; si falla, el pod sale del
	// balanceador pero sigue vivo.
	//
	// Si /salud comprobara la base de datos, una caída de PostgreSQL
	// reiniciaría los 45 pods de ARQ-03 en cadena, justo cuando lo que hace
	// falta es que esperen a que vuelva.
	s.rutas.HandleFunc("GET /salud", s.manejarSalud)
	s.rutas.HandleFunc("GET /listo", s.manejarListo)
	s.rutas.Handle("GET /metrics", promhttp.HandlerFor(
		metricas.Registro, promhttp.HandlerOpts{Registry: metricas.Registro},
	))

	return s
}

// Registrar monta un manejador en una ruta. El patrón sigue la sintaxis de
// net/http desde Go 1.22: "POST /v1/reservas".
func (s *Servidor) Registrar(patron string, manejador http.Handler) {
	s.rutas.Handle(patron, manejador)
}

// RegistrarFunc es Registrar para funciones.
func (s *Servidor) RegistrarFunc(patron string, manejador http.HandlerFunc) {
	s.rutas.HandleFunc(patron, manejador)
}

// Metricas expone los instrumentos comunes para que la capa de transporte
// pueda instrumentar sus rutas. El registro de Prometheus sigue siendo del
// servidor: esto da acceso a los contadores, no permite crear series nuevas
// fuera de NuevasMetricas.
func (s *Servidor) Metricas() *Metricas {
	return s.metricas
}

// AnadirComprobacion suma una dependencia a la sonda de disponibilidad.
func (s *Servidor) AnadirComprobacion(c Comprobacion) {
	s.comprobaciones = append(s.comprobaciones, c)
}

// Escuchar arranca el servidor y bloquea hasta que se cancela el contexto.
//
// Al cancelarse no corta: deja de aceptar conexiones nuevas y espera a que
// terminen las que están en curso, hasta TiempoApagado. Sin esto, un
// despliegue rodante aborta transacciones a medio confirmar en cada pod que
// se sustituye.
func (s *Servidor) Escuchar(ctx context.Context) error {
	servidor := &http.Server{
		Addr:    s.cfg.Direccion,
		Handler: s.rutas,

		// Sin estos plazos una conexión lenta retiene un manejador
		// indefinidamente, y con suficientes de ellas el pod deja de atender
		// sin que ninguna sonda lo note.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errores := make(chan error, 1)
	go func() {
		s.registro.Info("escuchando", slog.String("direccion", s.cfg.Direccion))
		if err := servidor.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errores <- fmt.Errorf("servidor http: %w", err)
			return
		}
		errores <- nil
	}()

	select {
	case err := <-errores:
		return err
	case <-ctx.Done():
		s.registro.Info("apagando", slog.Duration("plazo", s.cfg.TiempoApagado))

		// Contexto propio: el que recibimos ya está cancelado, así que usarlo
		// para el apagado lo abortaría de inmediato y no esperaría a nadie.
		ctxApagado, cancelar := context.WithTimeout(context.Background(), s.cfg.TiempoApagado)
		defer cancelar()

		// contextcheck marca el contexto nuevo, pero aquí es justo lo que
		// hace falta: heredar del que ya está cancelado abortaría el
		// apagado antes de esperar a nadie.
		//nolint:contextcheck // contexto nuevo deliberado, ver arriba
		if err := servidor.Shutdown(ctxApagado); err != nil {
			return fmt.Errorf("apagado: %w", err)
		}
		s.registro.Info("apagado limpio")
		return nil
	}
}

func (s *Servidor) manejarSalud(w http.ResponseWriter, r *http.Request) {
	s.responderJSON(w, r, http.StatusOK, map[string]string{"estado": "vivo"})
}

func (s *Servidor) manejarListo(w http.ResponseWriter, r *http.Request) {
	ctx, cancelar := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancelar()

	resultados := make(map[string]string, len(s.comprobaciones))
	estado := http.StatusOK

	for _, c := range s.comprobaciones {
		if err := c.Verificar(ctx); err != nil {
			resultados[c.Nombre] = "error: " + err.Error()
			estado = http.StatusServiceUnavailable
			continue
		}
		resultados[c.Nombre] = "ok"
	}

	s.responderJSON(w, r, estado, resultados)
}

func (s *Servidor) responderJSON(w http.ResponseWriter, r *http.Request, estado int, cuerpo any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(estado)
	if err := json.NewEncoder(w).Encode(cuerpo); err != nil {
		// La cabecera ya salió, así que no se puede cambiar la respuesta.
		// Queda registrarlo.
		s.registro.ErrorContext(r.Context(), "no se pudo escribir la respuesta",
			slog.String("error", err.Error()))
	}
}
