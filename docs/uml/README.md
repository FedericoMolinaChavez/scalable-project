# UML Diagrams

PlantUML sources for the system design: one file per functional requirement, plus the data model.

## Structure

```
docs/uml/
  requirements/     one .puml file per functional requirement (RF-01 … RF-38)
  modelo-datos/     entity-relationship diagrams (ER-01 … ER-03)
  rendered/         generated images (gitignored, recreate with the render script)
```

The render script picks up every `.puml` under `docs/uml/` recursively, so new
subfolders work without touching it.

## Naming convention

`<ID>-<short-slug>.puml`, matching the ID from the spec — e.g. `RF-01-reserva-con-pago.puml`,
`ER-01-nucleo-reservas.puml`.

## Template

```plantuml
@startuml RF-01-nombre-requisito

title RF-01 - Nombre del requisito

' diagram content here

@enduml
```

## Conventions used across these diagrams

### ER diagrams (`modelo-datos/`)

- `entity` blocks with `hide circle` + `skinparam linetype ortho`.
- Every tenant-scoped table carries `tenant_id` as part of its primary key, because the
  tables are `PARTITION BY HASH (tenant_id)` and Postgres requires unique constraints to
  include the partition key.
- Entities defined in another ER file are repeated as a stub with `...` and a
  "Definida en ER-0X" note, rather than duplicating their columns.
- Constraints that carry a requirement's meaning (EXCLUDE, CHECK, UNIQUE) are written
  verbatim in the entity body — they are the requirement, enforced by the engine.

### Activity diagrams (`requirements/`)

- Activity diagrams with swimlanes (`|Actor|`), one per requirement, mirroring its actor(s).
- A 2-way decision → `if/then/else/endif`; a single question with 3+ outcomes → `switch/case/endswitch` (avoid chained `elseif`, which creates a new nested question per branch instead of one multi-way fork) — **unless** two or more `case`s cross into the same swimlane, see the bug note below.
- Prefer at most one swimlane crossing per branch of an `if/else`. Branches that ping-pong between lanes multiple times (e.g. a confirm-dialog round trip) tangle badly against a sibling branch doing the same — collapse the round trip into one crossing instead (e.g. "selecciona y confirma" as a single step) even if it drops a minor "user cancels the dialog" alt-flow.
- Each rejected/error path ends in its own `stop` rather than merging back into the happy path.
- Alternate flows described in the spec as "applies at any step" (cross-cutting exceptions, e.g. rate limiting, retries) are listed in a `legend right ... end legend` block instead of being drawn as branches on every step — keeps the happy-path/error-path structure readable. Note: a floating `note as N ... end note` placed after a diagram's final `stop` fails to render in this PlantUML version; `legend` works because it isn't tied to activity-flow state.

### Known PlantUML bugs (this version, activity + swimlanes)

- **`switch`/`case` crashes with swimlanes**: if two or more `case` branches cross into the *same* lane and at least one of them ends in an unconditional `stop`, rendering throws `java.lang.IllegalArgumentException` in `FtileSwitchNude.getTranslateNude`. Repro confirmed minimally (two cases, one lane change each into lane `B`, one case ending in `stop`). Workaround: use nested `if/else` instead of `switch/case` whenever a branch needs to dead-end in a shared lane (see RF-17).
- Even without crashing, `switch/case` and `if/elseif` across swimlanes can render with tangled/overlapping wires when a branch crosses lanes more than once. If a rendered diagram looks tangled, it's a layout issue, not a spec error — restructure per the two bullets above rather than trying to fix it with more `note`/spacing tricks.

## Rendering

Requires only a JVM (Java 11+) — already satisfied on this machine.

- **Script (recommended):** renders every `.puml` in `requirements/` in one go.
  ```bash
  scripts/render-uml.sh                       # -> docs/uml/rendered/*.png
  scripts/render-uml.sh /path/elsewhere svg    # custom output dir + format
  ```
  ```powershell
  .\scripts\render-uml.ps1
  .\scripts\render-uml.ps1 -OutputDir C:\path\elsewhere -Format svg
  ```
  Both scripts call `tools/plantuml.jar` (downloaded from the [official PlantUML releases](https://github.com/plantuml/plantuml/releases), gitignored — re-download if missing: `curl -L -o tools/plantuml.jar https://github.com/plantuml/plantuml/releases/latest/download/plantuml.jar`).
- **VS Code:** install the "PlantUML" extension (jebbs.plantuml) and preview with `Alt+D`.
- **Single file via CLI:** `java -jar tools/plantuml.jar docs/uml/requirements/RF-01-reserva-con-pago.puml`.

`docs/uml/rendered/` is gitignored — it's build output, not source. Point the script's output-dir argument elsewhere if you want the images to land directly in another project/location.
