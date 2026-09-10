# Sistema de reservas — monorepo

Plataforma de reservas multi-tenant. El diseño está en `docs/uml/`; el modelo
físico, en `db/`. Este README solo explica **dónde vive cada cosa** y cómo
arrancar.

## Estructura

| Directorio | Contenido |
|---|---|
| `docs/uml/` | Requisitos (RF), arquitectura (ARQ) y modelo de datos (ER) en PlantUML |
| `db/` | Migraciones SQL, semillas y pruebas de invariantes del motor |
| `api/` | `openapi.yaml` — contrato único entre backend y frontend |
| `backend/` | Módulo Go: un binario por componente de ARQ-01 |
| `frontend/` | Aplicación React + TypeScript (Vite). Su sistema visual está en [DESIGN.md](DESIGN.md) |
| `deploy/` | Composición de la infraestructura local |
| `scripts/` | Utilidades (renderizado de UML) |

## Requisitos

- **Go 1.27+** — `winget install GoLang.Go`
- **Node 24** — con `fnm`, el archivo `.node-version` lo selecciona solo
- **Docker** — para la infraestructura local y las pruebas de base de datos
- **Task** — `winget install Task.Task` (ejecutor de tareas, ver `Taskfile.yml`)
- **golangci-lint v2** — `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`

Tras instalar Task y golangci-lint hay que abrir una terminal nueva: quedan en
`%LOCALAPPDATA%\Microsoft\WinGet\Links` y en `%USERPROFILE%\go\bin`, y el `PATH`
de la sesión en curso no los ve todavía.

## Sobre `task back:test`

La tarea normal corre `go test ./...` sin más. El detector de carreras vive
aparte, en `back:test:race`, porque exige CGO y un compilador C que Windows no
trae. Es la variante que de verdad importa —45 pods escribiendo sin coordinarse
(ARQ-03) no perdonan una carrera—, así que va obligatoria en CI sobre Linux, no
opcional.

## Arranque

```bash
cp .env.example .env
task infra:up
task back:run -- consulta       # :8081 — lecturas
task back:run -- nucleo         # :8080 — escrituras
task back:run -- identidad      # :8082 — códigos y tokens
task back:run -- pagos          # :8084 — Stripe, en las dos direcciones
task back:run -- trabajadores   # :8083 — los ocho bucles asíncronos
task front:dev
```

Cada servicio va en su terminal, y son cinco y no uno por la razón de ARQ-01: se
separa por **frontera transaccional y dominio de fallo**. La escritura de una
reserva debe ser atómica; la lectura no; la identidad manda correo, así que un
relé caído no puede arrastrar consigo la ruta de reserva; y los pagos dependen
de Stripe, cuya disponibilidad no controlamos.

`pagos` y `trabajadores` se niegan a arrancar sin las claves de Stripe. Es
deliberado: un servicio que no comprueba la firma de un webhook confirma
reservas que nadie pagó. Para trabajar sin ellas, no los levantes — el resto del
sistema funciona igual, que es justamente lo que la separación compra.

La consecuencia es que `/v1/reservas` lo sirven dos procesos —el `POST` el
núcleo, el `GET` el de consulta— y el proxy de Vite enruta por método y por
prefijo. Está explicado en [backend/README.md](backend/README.md) y en
`frontend/vite.config.ts`.

## Pagos (RF-01, RF-33)

Hacen falta tres claves de prueba en el `.env` (ver `.env.example`). Las dos
primeras salen del panel de Stripe en modo prueba; la tercera la imprime la CLI:

```bash
task stripe:escuchar   # stripe listen --forward-to localhost:8084/v1/webhooks/stripe
```

Ese comando imprime un `whsec_...` al arrancar: ese es el `STRIPE_WEBHOOK_SECRET`
mientras la sesión de escucha dure. Es **distinto** del secreto del endpoint
configurado en el panel, y se regenera en cada sesión.

Con eso, el flujo completo se puede recorrer con las tarjetas de prueba de
Stripe: reservar (el núcleo aparta el cupo y arranca el reloj de RF-27), pagar
en la página, y ver cómo la reserva pasa a `confirmada` cuando llega el webhook.
Si el webhook no llega, el conciliador acaba preguntándole a Stripe por su
cuenta —esa es la razón de que exista— y la reserva se confirma igual.

El correo de desarrollo lo captura Mailpit en <http://localhost:8025>: ahí se
leen el código de RF-02 y los avisos de RF-10 sin que salga nada fuera. Los
comprobantes de RF-34 van a MinIO, cuya consola está en
<http://localhost:9001>.

`task` sin argumentos lista todo lo disponible. Los detalles de la
infraestructura local están en [deploy/README.md](deploy/README.md).

La base de datos solo es alcanzable por el 6432 de PgBouncer: la composición no
publica el 5432 a propósito, para que el desarrollo no pueda esquivar el pooler
y las restricciones del modo transacción se noten aquí y no en producción.

## Antes de un push

```bash
task ci
```

En GitHub corre además `task back:test:race` y `task db:test`, que en local son
opcionales: el detector de carreras exige CGO y un compilador C que Windows no
trae. Ver `.github/workflows/ci.yml`.

## Por qué un solo módulo Go

Los siete componentes de ARQ-01 comparten dominio y modelo de datos. Separarlos
en módulos obligaría a versionar código compartido que todavía se mueve. Cada
uno es un binario independiente en `cmd/`, así que el despliegue sigue siendo
por servicio; solo el desarrollo es conjunto.
