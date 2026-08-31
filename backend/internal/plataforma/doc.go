// Package plataforma contiene el arranque común a todos los binarios de cmd/:
// carga de configuración, logger estructurado (slog en JSON hacia Loki),
// métricas de Prometheus, trazas de OpenTelemetry hacia Tempo, servidor HTTP
// con /salud y /metrics, y apagado limpio.
//
// Ningún servicio construye su propio arranque: todos llaman aquí. Es lo que
// garantiza que los tres pilares de observabilidad de ARQ-01 existan en cada
// componente desde el primer commit y no se añadan después servicio por
// servicio.
package plataforma
