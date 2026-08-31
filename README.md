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
task back:run -- consulta   # :8081 — lecturas
task back:run -- nucleo     # :8080 — escrituras
task front:dev
```

Los dos servicios van en terminales distintas, y son dos y no uno por la razón
de ARQ-01: la escritura y la lectura tienen fronteras transaccionales
distintas. `/v1/reservas` lo sirven ambos —el `POST` el núcleo, el `GET` el de
consulta—, y el proxy de Vite enruta por método. Está explicado en
[backend/README.md](backend/README.md) y en `frontend/vite.config.ts`.

`task` sin argumentos lista todo lo disponible. Los detalles de la
infraestructura local están en [deploy/README.md](deploy/README.md).

La base de datos solo es alcanzable por el 6432 de PgBouncer: la composición no
publica el 5432 a propósito, para que el desarrollo no pueda esquivar el pooler
y las restricciones del modo transacción se noten aquí y no en producción.

## Antes de un push

```bash
task ci
```

## Por qué un solo módulo Go

Los siete componentes de ARQ-01 comparten dominio y modelo de datos. Separarlos
en módulos obligaría a versionar código compartido que todavía se mueve. Cada
uno es un binario independiente en `cmd/`, así que el despliegue sigue siendo
por servicio; solo el desarrollo es conjunto.
