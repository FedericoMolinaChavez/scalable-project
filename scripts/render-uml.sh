#!/usr/bin/env bash
# Renders every .puml source under docs/uml (recursively) to images.
#
# Usage:
#   scripts/render-uml.sh [output-dir] [format]
#
#   output-dir  defaults to docs/uml/rendered (absolute or relative to cwd)
#   format      png (default) or svg
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

PLANTUML_JAR="$REPO_ROOT/tools/plantuml.jar"
SOURCE_DIR="$REPO_ROOT/docs/uml"
OUTPUT_DIR="${1:-$REPO_ROOT/docs/uml/rendered}"
FORMAT="${2:-png}"

if [ ! -f "$PLANTUML_JAR" ]; then
  echo "plantuml.jar not found at $PLANTUML_JAR" >&2
  exit 1
fi

mkdir -p "$OUTPUT_DIR"

# Collect sources from every subdirectory except the render output itself.
files=()
while IFS= read -r f; do
  files+=("$f")
done < <(find "$SOURCE_DIR" -name '*.puml' -not -path '*/rendered/*' | sort)

if [ ${#files[@]} -eq 0 ]; then
  echo "No .puml files found under $SOURCE_DIR" >&2
  exit 0
fi

java -jar "$PLANTUML_JAR" "-t${FORMAT}" -o "$OUTPUT_DIR" "${files[@]}"

echo "Rendered ${#files[@]} diagram(s) to $OUTPUT_DIR"
