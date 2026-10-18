#!/bin/sh
set -eu
if [ ! -f .local/root.key ] || [ ! -f .local/postgres-password ]; then
  printf '%s\n' 'Run scripts/init-dev.sh first.' >&2
  exit 1
fi
ANT_DATABASE_URL="postgres://ant:$(cat .local/postgres-password)@127.0.0.1:15432/ant?sslmode=disable"
ANT_ROOT_KEY_FILE=.local/root.key
export ANT_DATABASE_URL ANT_ROOT_KEY_FILE
exec go run ./cmd/ant "$@"
