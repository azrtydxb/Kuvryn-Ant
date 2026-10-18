#!/bin/sh
# Isolated, temporary PostgreSQL fixture using an existing local installation.
set -eu
fixture=$(mktemp -d "${TMPDIR:-/tmp}/ant-postgres.XXXXXX")
cleanup() {
  pg_ctl -D "$fixture/data" -m immediate stop >/dev/null 2>&1 || true
  rm -rf "$fixture"
}
trap cleanup EXIT HUP INT TERM
initdb -D "$fixture/data" -A trust -U ant --no-locale >"$fixture/init.log"
# Unix socket only: no TCP listener or changes to an existing database.
pg_ctl -D "$fixture/data" -l "$fixture/server.log" -o "-k $fixture -h ''" -w start >/dev/null
createdb -h "$fixture" -U ant ant_test
if [ "$#" -eq 0 ]; then set -- ./...; fi
ANT_TEST_DATABASE_URL="host=$fixture user=ant dbname=ant_test sslmode=disable" ANT_REQUIRE_INTEGRATION=1 go test -race -count=1 "$@"
