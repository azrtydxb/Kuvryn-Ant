#!/bin/sh
# Usage: conformance/run.sh <target> [formats...]
#   target   local | oke | kw | <any label for the results directory>
# Environment:
#   ANT_BASE_URL     base URL (local: auto-started if unset; oke: https://ant.azrty.com; kw: https://ant.kw.watteel.lab)
#   ANT_TOKEN / ANT_TOKEN_FILE   admin service token (never written to results)
#   ANT_CA_BUNDLE    PEM bundle for a private CA (kw)
#   ANT_VERSION      version label recorded as Ant version (default: git sha)
# Needs: docker, python3 (stdlib only).
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
[ $# -ge 1 ] || {
	echo "usage: $0 <target> [formats...]" >&2
	exit 2
}
TARGET=$1
case "$TARGET" in
oke) : "${ANT_BASE_URL:=https://ant.azrty.com}" ;;
kw) : "${ANT_BASE_URL:=https://ant.kw.watteel.lab}" ;;
esac
STARTED=
if [ "$TARGET" = local ] && [ -z "${ANT_BASE_URL:-}" ]; then
	eval "$("$HERE/dev-instance.sh" start | tail -2)"
	STARTED=1
fi
export ANT_BASE_URL
[ -z "${ANT_TOKEN_FILE:-}" ] || export ANT_TOKEN_FILE
set +e
python3 -I "$HERE/harness/run.py" "$@"
RC=$?
[ -n "$STARTED" ] && [ -z "${ANT_KEEP_INSTANCE:-}" ] && "$HERE/dev-instance.sh" stop >/dev/null
exit $RC
