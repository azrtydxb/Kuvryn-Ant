#!/bin/sh
# Start (or stop) a private local Kuvryn Ant instance for the conformance harness.
#   conformance/dev-instance.sh start   -> prints "export ANT_BASE_URL=... ANT_TOKEN_FILE=..." lines
#   conformance/dev-instance.sh stop
# State lives in $ANT_CONF_STATE (default /tmp/ant-conformance-dev). Needs initdb/pg_ctl/createdb and go.
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
STATE=${ANT_CONF_STATE:-/tmp/ant-conformance-dev}
case "${1:-start}" in
stop)
	[ -f "$STATE/ant.pid" ] && kill "$(cat "$STATE/ant.pid")" 2>/dev/null || true
	[ -d "$STATE/pg" ] && pg_ctl -D "$STATE/pg" -m immediate stop >/dev/null 2>&1 || true
	rm -rf "$STATE"
	echo stopped
	;;
start)
	[ -f "$STATE/env" ] && {
		cat "$STATE/env"
		exit 0
	}
	umask 077
	mkdir -p "$STATE"
	openssl rand -out "$STATE/root.key" 32
	initdb -D "$STATE/pg" -A trust -U ant --no-locale >/dev/null
	pg_ctl -D "$STATE/pg" -l "$STATE/pg.log" -o "-k $STATE -h ''" -w start >/dev/null
	createdb -h "$STATE" -U ant ant_conf
	(cd "$ROOT" && go build -o "$STATE/ant" ./cmd/ant)
	export ANT_DATABASE_URL="host=$STATE user=ant dbname=ant_conf sslmode=disable"
	export ANT_ROOT_KEY_FILE="$STATE/root.key" ANT_DATA_DIR="$STATE/storage" ANT_STORAGE_BACKEND=disk
	"$STATE/ant" bootstrap conformance >"$STATE/credential.json"
	PORT=${ANT_CONF_PORT:-$(awk 'BEGIN{srand(); print int(20000+rand()*30000)}')}
	ANT_LISTEN="127.0.0.1:$PORT" nohup "$STATE/ant" >"$STATE/app.log" 2>&1 &
	echo $! >"$STATE/ant.pid"
	i=0
	until curl -fsS "http://127.0.0.1:$PORT/health/ready" >/dev/null 2>&1; do
		i=$((i + 1))
		[ $i -gt 100 ] && {
			echo "ant did not become ready; see $STATE/app.log" >&2
			exit 1
		}
		sleep 0.2
	done
	sed -n 's/.*"token"[ ]*:[ ]*"\([^"]*\)".*/\1/p' "$STATE/credential.json" >"$STATE/token"
	printf 'export ANT_BASE_URL=http://127.0.0.1:%s\nexport ANT_TOKEN_FILE=%s/token\n' "$PORT" "$STATE" >"$STATE/env"
	cat "$STATE/env"
	;;
esac
