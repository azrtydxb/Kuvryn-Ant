#!/bin/sh
# Federation unit tests and the multi-instance harness (several instances, each
# with its own database in one disposable PostgreSQL fixture, real mTLS
# federation listeners and contract-validated management APIs).
set -eu
cd "$(dirname "$0")/.."
if [ "$#" -eq 0 ]; then set -- ./internal/federation/...; fi
exec scripts/test-postgres.sh "$@"
