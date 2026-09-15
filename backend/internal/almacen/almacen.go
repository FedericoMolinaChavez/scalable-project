// Package almacen es la puerta al almacenamiento de objetos de ARQ-01 (MinIO).
//
// Su único inquilino son los comprobantes de RF-34, y esa es la razón de que el
// documento no viva en PostgreSQL: un comprobante es un archivo de decenas de
// kilobytes que se escribe una vez y se lee poco, y meterlo en una fila lo
// arrastra a cada copia de seguridad, a cada réplica y al caché de páginas del
// motor, compitiendo por memoria con las tablas que sí están en la ruta
// caliente.
//
// Lo que sí queda en PostgreSQL es la clave del objeto, no la URL. Una URL
// firmada lleva la firma dentro y caduca: guardarla sería guardar una
// credencial con fecha de caducidad en una columna que se lee para siempre.
package almacen

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// Cubo es el bucket donde viven los comprobantes.
//
// Uno solo para todos los tenants, con el tenant en el prefijo de la clave. Un
// bucket por tenant parece más aislado y no lo es: S3 limita el número de
// buckets por cuenta, crearlos exige un permiso administrativo en la ruta de
// alta de un negocio, y el aislamiento real —quién puede leer qué— se decide
// igualmente al firmar la URL, no en el nombre del contenedor.
const Cubo = "comprobantes"

// Almacen habla con MinIO.
type Almacen struct {
	cliente *minio.Client
}

// Config es lo que hace falta para llegar al almacén.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Seguro    bool
}

// Abrir crea el cliente y se asegura de que el cubo existe.
//
// La creación es idempotente, así que arrancar varias réplicas a la vez no es
// un problema: es el mismo criterio que el relay aplica al flujo de JetStream.
func Abrir(ctx context.Context, cfg Config) (*Almacen, error) {
	cliente, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Seguro,
	})
	if err != nil {
		return nil, fmt.Errorf("no se pudo abrir el almacén en %s: %w", cfg.Endpoint, err)
	}

	existe, err := cliente.BucketExists(ctx, Cubo)
	if err != nil {
		return nil, fmt.Errorf("no se pudo comprobar el cubo %s: %w", Cubo, err)
	}
	if !existe {
		if err := cliente.MakeBucket(ctx, Cubo, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("no se pudo crear el cubo %s: %w", Cubo, err)
		}
	}

	return &Almacen{cliente: cliente}, nil
}

// Comprobar es la verificación para la sonda de disponibilidad.
func (a *Almacen) Comprobar(ctx context.Context) error {
	if _, err := a.cliente.BucketExists(ctx, Cubo); err != nil {
		return fmt.Errorf("almacén de objetos: %w", err)
	}
	return nil
}

// Guardar sube un documento y devuelve su clave.
func (a *Almacen) Guardar(
	ctx context.Context, clave string, contenido []byte, tipoMedio string,
) error {
	_, err := a.cliente.PutObject(ctx, Cubo, clave,
		strings.NewReader(string(contenido)), int64(len(contenido)),
		minio.PutObjectOptions{ContentType: tipoMedio})
	if err != nil {
		return fmt.Errorf("no se pudo guardar %s: %w", clave, err)
	}
	return nil
}

// EnlaceFirmado devuelve una URL temporal para descargar un objeto.
//
// No es una llamada de red: la firma se calcula en local con la clave secreta,
// así que esto se puede hacer en la ruta de una petición sin sumar latencia de
// MinIO ni depender de que esté en pie para responder.
//
// La vigencia es corta a propósito. El enlace ES la credencial: quien lo tenga
// puede descargar el documento, y un enlace que dura un día es un documento que
// se reenvía por chat y sigue abriéndose mañana.
func (a *Almacen) EnlaceFirmado(
	ctx context.Context, clave string, vigencia time.Duration,
) (string, time.Time, error) {
	enlace, err := a.cliente.PresignedGetObject(ctx, Cubo, clave, vigencia, url.Values{})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("no se pudo firmar el enlace a %s: %w", clave, err)
	}

	return enlace.String(), time.Now().Add(vigencia), nil
}

// Clave arma la ruta de un comprobante dentro del cubo.
//
// El tenant va primero para que un prefijo baste como unidad de política: una
// regla de ciclo de vida, una migración o un borrado por tenant se expresan
// sobre `comprobantes/<tenant>/` sin tener que listar objeto por objeto.
func Clave(tenant, comprobante string) string {
	return tenant + "/" + comprobante + ".html"
}
