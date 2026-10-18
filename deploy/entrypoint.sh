#!/bin/sh
set -eu
umask 077
# Compose supplies a password file; PostgreSQL's pgx client reads PGPASSWORD.
# Production chart supplies the complete DSN through a Secret environment entry.
if [ -n "${ANT_DATABASE_PASSWORD_FILE:-}" ]; then
  PGPASSWORD=$(cat "$ANT_DATABASE_PASSWORD_FILE")
  export PGPASSWORD
fi
mkdir -p "$ANT_DATA_DIR"
chmod 0700 "$ANT_DATA_DIR"
exec /usr/local/bin/ant "$@"
