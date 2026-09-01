// Package correo entrega correo por SMTP.
//
// Es lo mínimo para RF-02: un mensaje de texto plano a un destinatario. El
// motor de notificaciones de RF-10 —plantillas, preferencias por canal
// (RF-21), reintentos, idempotencia— es un trabajador asíncrono que todavía no
// existe, y cuando exista este paquete será su transporte, no su sustituto.
//
// En desarrollo el destino es Mailpit, que captura todo en
// http://localhost:8025 y no entrega nada fuera. Que sea imposible mandarle un
// correo de verdad a alguien mientras se programa no es un detalle de
// comodidad: es lo que permite probar el flujo de RF-02 con direcciones
// inventadas sin riesgo.
package correo

import (
	"context"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// Emisor manda correo.
type Emisor interface {
	Enviar(ctx context.Context, mensaje Mensaje) error
}

// Mensaje es un correo de texto plano.
type Mensaje struct {
	Para   string
	Asunto string
	Cuerpo string
}

// SMTP entrega contra un servidor SMTP.
type SMTP struct {
	direccion string
	remitente string
	registro  *slog.Logger
}

func NuevoSMTP(host, puerto, remitente string, registro *slog.Logger) *SMTP {
	return &SMTP{
		direccion: net.JoinHostPort(host, puerto),
		remitente: remitente,
		registro:  registro,
	}
}

// Enviar entrega el mensaje.
//
// Sin autenticación ni TLS: el destino en desarrollo es Mailpit, que no pide
// ninguna de las dos. Contra un relé real hacen falta ambas, y ese es el
// cambio que toca al desplegar, no al escribir esto.
func (s *SMTP) Enviar(ctx context.Context, mensaje Mensaje) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	cuerpo := construir(s.remitente, mensaje)

	// DialContext y no DialTimeout: además del plazo propio, la conexión muere
	// si el contexto de la petición se cancela. Un plazo suelto seguiría
	// esperando cinco segundos a un relé caído después de que quien pidió el
	// código ya cerrara la pestaña, reteniendo un manejador para nadie.
	marcador := &net.Dialer{Timeout: 5 * time.Second}

	conexion, err := marcador.DialContext(ctx, "tcp", s.direccion)
	if err != nil {
		return fmt.Errorf("no se pudo conectar a SMTP en %s: %w", s.direccion, err)
	}

	cliente, err := smtp.NewClient(conexion, s.direccion)
	if err != nil {
		_ = conexion.Close()
		return fmt.Errorf("no se pudo abrir la sesión SMTP: %w", err)
	}
	defer func() { _ = cliente.Close() }()

	if err := cliente.Mail(s.remitente); err != nil {
		return fmt.Errorf("SMTP rechazó el remitente: %w", err)
	}
	if err := cliente.Rcpt(mensaje.Para); err != nil {
		return fmt.Errorf("SMTP rechazó el destinatario: %w", err)
	}

	escritor, err := cliente.Data()
	if err != nil {
		return fmt.Errorf("SMTP rechazó el cuerpo: %w", err)
	}
	if _, err := escritor.Write([]byte(cuerpo)); err != nil {
		return fmt.Errorf("no se pudo escribir el mensaje: %w", err)
	}
	if err := escritor.Close(); err != nil {
		return fmt.Errorf("no se pudo cerrar el mensaje: %w", err)
	}

	return cliente.Quit()
}

// construir arma el mensaje RFC 5322.
//
// Los saltos son CRLF porque el protocolo lo exige, y el asunto va codificado
// en UTF-8 explícitamente: sin eso, un asunto con tildes —que en español es
// casi cualquiera— llega como caracteres rotos en varios clientes.
func construir(remitente string, mensaje Mensaje) string {
	var b strings.Builder

	b.WriteString("From: " + remitente + "\r\n")
	b.WriteString("To: " + mensaje.Para + "\r\n")
	b.WriteString("Subject: " + asuntoCodificado(mensaje.Asunto) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("\r\n")
	b.WriteString(strings.ReplaceAll(mensaje.Cuerpo, "\n", "\r\n"))

	return b.String()
}

// asuntoCodificado envuelve el asunto en la codificación de RFC 2047 cuando
// lleva algo que no sea ASCII.
//
// Solo cuando hace falta: un asunto ASCII codificado igualmente es legible para
// las máquinas pero ilegible para quien mire el correo en crudo, y en
// desarrollo se mira en crudo a menudo.
func asuntoCodificado(asunto string) string {
	if esASCII(asunto) {
		return asunto
	}
	return mime.QEncoding.Encode("UTF-8", asunto)
}

func esASCII(s string) bool {
	for i := range len(s) {
		if s[i] > 127 {
			return false
		}
	}
	return true
}
