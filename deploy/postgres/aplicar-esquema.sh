#!/bin/sh
# Aplica el esquema al inicializar el contenedor.
#
# Solo corre la primera vez, cuando el volumen de datos esta vacio: es como
# funciona docker-entrypoint-initdb.d. Para reaplicar desde cero hace falta
# borrar el volumen (task infra:reset).
#
# Replica exactamente el orden que documenta db/README.md: las siete
# migraciones numeradas y despues la semilla, que es la que crea el rol de
# conexion app_dev. Sin ese ultimo paso no hay usuario con el que conectarse.
set -eu

psql_root() {
    psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -q "$@"
}

echo "==> Aplicando migraciones"
for f in /db/migrations/*.sql; do
    printf '    %s\n' "$(basename "$f")"
    psql_root -f "$f"
done

echo "==> Sembrando datos de desarrollo"
psql_root -f /db/semillas/dev.sql

echo "==> Esquema listo"
