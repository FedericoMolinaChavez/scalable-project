package transporte

import (
	"context"
	"net/http"

	"github.com/FedericoMolinaChavez/scalable-project/backend/internal/api"
)

// Clase es un tipo de problema de RFC 9457: el par estable `type` + `title`,
// junto al estado HTTP que le corresponde.
//
// Van juntos a propósito. El contrato dice que `title` es "constante para un
// mismo type", y esa constancia es lo que permite a un cliente ramificar sobre
// `type` sin leer prosa. Construyendo el problema desde una Clase, el título no
// puede divergir entre dos manejadores que devuelven el mismo tipo, que es
// exactamente lo que pasa cuando cada uno escribe su literal.
type Clase struct {
	Tipo   string
	Titulo string
	Estado int
}

// Las clases que esta rebanada puede devolver. El identificador es una URI
// estable, no una URL que haya que resolver: RFC 9457 no exige que se pueda
// descargar nada de ahí.
var (
	Invalida = Clase{
		Tipo:   "https://api.reservas/errores/peticion-invalida",
		Titulo: "La petición está mal formada",
		Estado: http.StatusBadRequest,
	}

	// NoEncontrado no distingue "no existe" de "existe en otro tenant", y no
	// es una imprecisión: bajo RLS ambos casos llegan al código como cero
	// filas, y separarlos filtraría la existencia de datos ajenos.
	NoEncontrado = Clase{
		Tipo:   "https://api.reservas/errores/no-encontrado",
		Titulo: "No existe",
		Estado: http.StatusNotFound,
	}

	// HorarioOcupado es la traducción del 23P01. No es un fallo del cliente ni
	// del servidor: es el resultado normal de perder una carrera por el mismo
	// cupo (RF-01, flujo alternativo 3).
	HorarioOcupado = Clase{
		Tipo:   "https://api.reservas/errores/horario-ocupado",
		Titulo: "El horario ya está reservado",
		Estado: http.StatusConflict,
	}

	// NoAutorizado no distingue entre "no mandaste token" y "el que mandaste
	// está mal". Para quien prueba tokens, saber cuál de las dos cosas falló es
	// la mitad del trabajo hecho.
	NoAutorizado = Clase{
		Tipo:   "https://api.reservas/errores/no-autorizado",
		Titulo: "Hace falta identificarse",
		Estado: http.StatusUnauthorized,
	}

	// TokenVencido sí es un tipo aparte, y no contradice lo anterior: caducar
	// no es un fallo de quien presenta el token, es el funcionamiento normal.
	// La interfaz necesita poder pedir que se identifique otra vez en vez de
	// tratarlo como un rechazo.
	TokenVencido = Clase{
		Tipo:   "https://api.reservas/errores/token-vencido",
		Titulo: "La identificación caducó",
		Estado: http.StatusUnauthorized,
	}

	// EstadoIncompatible: la operación es válida y quien la pide tiene derecho,
	// pero el recurso ya no está en un estado que la admita. Cancelar algo ya
	// cancelado no es un error de la petición ni una regla de negocio violada:
	// es una carrera con otra cosa que ya pasó.
	EstadoIncompatible = Clase{
		Tipo:   "https://api.reservas/errores/estado-incompatible",
		Titulo: "La reserva ya no está en ese estado",
		Estado: http.StatusConflict,
	}

	ReglaNegocio = Clase{
		Tipo:   "https://api.reservas/errores/regla-de-negocio",
		Titulo: "La petición es válida pero viola una regla del negocio",
		Estado: http.StatusUnprocessableEntity,
	}

	// NoImplementado cubre lo que el contrato ya declara y esta rebanada
	// todavía no resuelve. Se responde 422 y no 501 porque el contrato no
	// declara 501 en ninguna ruta, y una respuesta fuera del contrato rompe
	// justo la garantía que el contrato existe para dar.
	NoImplementado = Clase{
		Tipo:   "https://api.reservas/errores/no-implementado",
		Titulo: "Esa parte del contrato todavía no está implementada",
		Estado: http.StatusUnprocessableEntity,
	}

	// DemasiadasPeticiones es el límite de RNF-08 y de RF-12 A11. Se distingue
	// de todo lo demás porque no es un fallo de quien lo recibe: es el sistema
	// pidiéndole que espere, y necesita saberlo para dejar de pulsar un botón
	// que ya no hace nada.
	DemasiadasPeticiones = Clase{
		Tipo:   "https://api.reservas/errores/demasiadas-peticiones",
		Titulo: "Demasiadas peticiones seguidas",
		Estado: http.StatusTooManyRequests,
	}

	Interno = Clase{
		Tipo:   "https://api.reservas/errores/error-interno",
		Titulo: "Error no previsto",
		Estado: http.StatusInternalServerError,
	}
)

// campoProblema es un alias del struct anónimo que el generador produce para
// `Problema.errores`. Alias y no tipo nuevo: así es el mismo tipo y se puede
// asignar al campo generado sin conversión.
type campoProblema = struct {
	Campo   string `json:"campo"`
	Mensaje string `json:"mensaje"`
}

// Campo es un error de validación atribuido a un campo concreto.
type Campo struct {
	Nombre  string
	Mensaje string
}

// Cuerpo construye el problema de esta clase.
//
// El detalle explica ESTA ocurrencia; el título dice de qué tipo es. Mantener
// la distinción importa: un cliente muestra el título y registra el detalle, y
// si el título variara con cada ocurrencia no podría agrupar nada.
func (c Clase) Cuerpo(ctx context.Context, detalle string) api.Problema {
	p := api.Problema{
		Type:   c.Tipo,
		Title:  c.Titulo,
		Status: c.Estado,
	}
	if detalle != "" {
		p.Detail = &detalle
	}

	// El identificador de la petición es lo que cruza esta respuesta con su
	// registro y su traza. Sin él, un informe de "me salió un 500" no se puede
	// buscar.
	if id := IdPeticion(ctx); id != "" {
		p.Instance = &id
	}

	return p
}

// CuerpoConCampos añade el detalle por campo de un error de validación.
func (c Clase) CuerpoConCampos(ctx context.Context, detalle string, campos ...Campo) api.Problema {
	p := c.Cuerpo(ctx, detalle)
	if len(campos) == 0 {
		return p
	}

	lista := make([]campoProblema, 0, len(campos))
	for _, campo := range campos {
		lista = append(lista, campoProblema{Campo: campo.Nombre, Mensaje: campo.Mensaje})
	}
	p.Errores = &lista

	return p
}

// Escribir emite el problema directamente sobre el ResponseWriter.
//
// Los manejadores de las rutas NO usan esto: devuelven el tipo de respuesta
// que el generador declara para su operación, y así el compilador comprueba
// que solo responden lo que el contrato permite. Esto es para los dos sitios
// que quedan fuera de ese alcance —el enlace de parámetros, que falla antes de
// llegar al manejador, y la recuperación de un pánico, que falla después—,
// donde igualmente hay que responder en el mismo formato para que un error sea
// indistinguible venga de donde venga.
func Escribir(w http.ResponseWriter, cuerpo api.Problema) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(cuerpo.Status)
	_ = escribirJSON(w, cuerpo)
}
