// Package plataforma contiene el arranque común a todos los binarios de cmd/:
// carga de configuración, logger estructurado (slog en JSON hacia Loki),
// métricas de Prometheus, servidor HTTP con /salud, /listo y /metrics, y
// apagado limpio.
//
// TRAZAS: todavía no hay. Este comentario prometía "trazas de OpenTelemetry
// hacia Tempo" y no existía ninguna dependencia de otel, así que decía algo
// falso sobre el paquete. Lo que sí hay es el identificador de petición que
// asigna internal/transporte, que cruza registros y respuestas.
//
// Faltan las dos mitades: el SDK aquí y un colector en
// deploy/docker-compose.yml, donde hoy no hay ni Tempo ni nada que reciba OTLP.
// Mientras esa segunda mitad no exista, instrumentar sería exportar a ninguna
// parte.
//
// Ningún servicio construye su propio arranque: todos llaman aquí. Es lo que
// garantiza que los tres pilares de observabilidad de ARQ-01 existan en cada
// componente desde el primer commit y no se añadan después servicio por
// servicio.
package plataforma
