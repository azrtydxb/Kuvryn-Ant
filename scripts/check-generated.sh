#!/bin/sh
# Compare with a temporary generation so checking never rewrites the working tree.
set -eu
cd "$(dirname "$0")/.."
workspace=$(mktemp -d "${TMPDIR:-/tmp}/ant-api-check.XXXXXX")
trap 'rm -rf "$workspace"' EXIT HUP INT TERM
# oapi-codegen gives the configured output precedence over -o.
sed '/^output:/d' api/codegen.yaml > "$workspace/codegen.yaml"
go tool oapi-codegen --config "$workspace/codegen.yaml" -o "$workspace/generated.go" api/openapi.yaml
if ! cmp -s api/generated.go "$workspace/generated.go"; then
  echo 'api/generated.go is stale; run go generate ./api and commit the result.' >&2
  diff -u api/generated.go "$workspace/generated.go" || true
  exit 1
fi
