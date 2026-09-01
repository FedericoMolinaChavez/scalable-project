package identidad

import "context"

// El acceso verificado viaja en el contexto de la petición.
//
// Tiene que ser así y no un parámetro: el generador produce manejadores que
// reciben ctx y el objeto de la petición, sin el *http.Request, así que la
// cabecera Authorization no les llega. El middleware la verifica una vez y
// deja el resultado aquí.
//
// La consecuencia importante es que un manejador NUNCA ve el token, solo lo
// que el token demostró. No puede reenviarlo, registrarlo por accidente ni
// guardarlo: no lo tiene.

type claveContexto int

const claveAcceso claveContexto = iota

// ConAcceso guarda el acceso ya verificado.
func ConAcceso(ctx context.Context, acceso Acceso) context.Context {
	return context.WithValue(ctx, claveAcceso, acceso)
}

// DeAcceso recupera el acceso de la petición en curso.
//
// El segundo valor es falso cuando no hay ninguno, y eso NO es un caso de
// error a ignorar: significa que la ruta se montó sin el middleware. Un
// manejador que lo dé por hecho serviría datos sin comprobar de quién son.
func DeAcceso(ctx context.Context) (Acceso, bool) {
	acceso, hay := ctx.Value(claveAcceso).(Acceso)
	return acceso, hay
}
