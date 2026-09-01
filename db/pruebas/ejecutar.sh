#!/usr/bin/env bash
# =============================================================================
# Levanta un PostgreSQL desechable, aplica el esquema y verifica las invariantes
# =============================================================================
# No monta volumenes: copia los archivos al contenedor con docker cp. Asi el
# guion funciona igual desde Git Bash en Windows que desde Linux, sin pelearse
# con la traduccion de rutas.
#
#   ./db/pruebas/ejecutar.sh            # esquema + invariantes + concurrencia
#   ./db/pruebas/ejecutar.sh --dejar    # deja el contenedor vivo al terminar
# =============================================================================
set -euo pipefail

# Git Bash reescribe cualquier argumento que parezca una ruta absoluta: un
# "/db/migrations/x.sql" destinado al contenedor llega como
# "C:/Program Files/Git/db/migrations/x.sql". Esto lo desactiva.
export MSYS_NO_PATHCONV=1
export MSYS2_ARG_CONV_EXCL='*'

CONTENEDOR="reservas-pruebas"
IMAGEN="postgres:17-alpine"
BD="reservas"
CLIENTES="${CLIENTES:-100}"
TRANSACCIONES="${TRANSACCIONES:-10}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# docker cp necesita la ruta de origen en formato nativo de Windows; el resto
# del guion sigue usando la forma POSIX.
REPO_ROOT_NATIVO="$(cd "$REPO_ROOT" && { pwd -W 2>/dev/null || pwd; })"
DEJAR=0
[[ "${1:-}" == "--dejar" ]] && DEJAR=1

limpiar() {
  if [[ $DEJAR -eq 0 ]]; then
    docker rm -f "$CONTENEDOR" >/dev/null 2>&1 || true
  else
    echo ""
    echo "Contenedor '$CONTENEDOR' en pie. Para entrar:"
    echo "  docker exec -it $CONTENEDOR psql -U app_dev -d $BD"
  fi
}
trap limpiar EXIT

psql_root() { docker exec -i "$CONTENEDOR" psql -U postgres -d "$BD" -v ON_ERROR_STOP=1 "$@"; }

echo "==> Levantando $IMAGEN"
docker rm -f "$CONTENEDOR" >/dev/null 2>&1 || true
# max_connections por encima del numero de clientes de pgbench: el default de
# 100 reserva slots para superusuario y la prueba de concurrencia se queda sin
# conexiones antes de llegar a la base. En produccion esto no aplica porque
# PgBouncer multiplexa (ARQ-01); aqui cada cliente abre la suya.
docker run -d --name "$CONTENEDOR" \
  -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB="$BD" \
  -p 55432:5432 "$IMAGEN" \
  -c max_connections=$((CLIENTES + 50)) >/dev/null

# -h localhost, y no es opcional.
#
# El punto de entrada de la imagen arranca un servidor TEMPORAL que escucha solo
# por el socket de Unix mientras inicializa el cluster, y despues lo para y
# levanta el definitivo. pg_isready por el socket da por buena esa fase: el
# guion sigue, empieza a aplicar migraciones, y a mitad el servidor temporal se
# apaga. El sintoma es "server closed the connection unexpectedly" en una
# migracion cualquiera, que no tiene nada que ver con la migracion.
#
# Forzando TCP solo responde el servidor definitivo. Es la misma leccion que ya
# estaba aprendida en deploy/docker-compose.yml y que aqui faltaba aplicar.
#
# Es una carrera, asi que pasa a veces: en local casi nunca y en CI si, porque
# el reparto de CPU es otro.
espera=0
until docker exec "$CONTENEDOR" pg_isready -h localhost -U postgres -d "$BD" >/dev/null 2>&1; do
  espera=$((espera + 1))
  if [[ $espera -gt 60 ]]; then
    echo "PostgreSQL no acepto conexiones TCP en 60 s" >&2
    docker logs "$CONTENEDOR" 2>&1 | tail -30 >&2
    exit 1
  fi
  sleep 1
done

echo "==> Copiando el esquema"
docker cp "$REPO_ROOT_NATIVO/db" "$CONTENEDOR:/db"

echo "==> Aplicando migraciones"
for f in "$REPO_ROOT"/db/migrations/*.sql; do
  nombre="$(basename "$f")"
  printf '    %s ... ' "$nombre"
  psql_root -q -f "/db/migrations/$nombre"
  echo "ok"
done

echo "==> Sembrando datos de desarrollo"
psql_root -q -f /db/semillas/dev.sql

echo ""
echo "==> Invariantes (como app_dev, sujeto a RLS)"
docker exec -i -e PGPASSWORD=dev "$CONTENEDOR" \
  psql -U app_dev -d "$BD" -v ON_ERROR_STOP=1 -q -f /db/pruebas/invariantes.sql

echo ""
echo "==> Concurrencia: $CLIENTES clientes peleando por el mismo cupo"
docker exec -i -e PGPASSWORD=dev "$CONTENEDOR" \
  pgbench -U app_dev -d "$BD" -n -f /db/pruebas/reserva_concurrente.sql \
  -c "$CLIENTES" -j 8 -t "$TRANSACCIONES" 2>&1 | tail -12

echo ""
echo "==> Filas efectivamente creadas sobre ese cupo"
psql_root -t -A -c "
  SELECT count(*)
  FROM negocio.reserva
  WHERE recurso_id = '44444444-4444-4444-4444-444444444444'
    AND periodo && tstzrange('2026-09-07 10:00-05','2026-09-07 11:00-05')
    AND estado IN ('pendiente','confirmada');" \
  | while read -r n; do
      total=$((CLIENTES * TRANSACCIONES))
      if [[ "$n" == "1" ]]; then
        echo "    $n  (de $total intentos)  <- la invariante de RNF-10 se sostiene"
      else
        echo "    $n  (de $total intentos)  <- FALLA: se esperaba exactamente 1"
        exit 1
      fi
    done
