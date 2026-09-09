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
| `frontend/` | Aplicación React + TypeScript (Vite) |
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
task back:run -- identidad      # :8082 — cuentas, sesiones y agentes
task back:run -- trabajadores   # :8083 — expirador, transiciones, relay
task front:dev
```

Cada servicio va en su terminal, y son tres y no uno por la razón de ARQ-01: se
separa por **frontera transaccional y dominio de fallo**. La escritura de una
reserva debe ser atómica; la lectura no; y la identidad manda correo, así que un
relé caído no puede arrastrar consigo la ruta de reserva.

La consecuencia es que `/v1/reservas` lo sirven dos procesos —el `POST` el
núcleo, el `GET` el de consulta— y el proxy de Vite enruta por método y por
prefijo. Está explicado en [backend/README.md](backend/README.md) y en
`frontend/vite.config.ts`.

El correo de desarrollo lo captura Mailpit en <http://localhost:8025>: ahí se
leen el código de RF-02, el enlace de verificación de una cuenta nueva (RF-19),
el magic link de RF-12 y el de recuperación de contraseña (RF-18), sin que salga
nada fuera. Los enlaces apuntan a `URL_APP`, que por defecto es el servidor de
Vite.

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
