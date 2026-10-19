#!/bin/sh
# Usage: conformance/run.sh <target> [formats...]
#   target   local | oke | kw | <any label for the results directory>
# Environment:
#   ANT_BASE_URL     base URL (local: auto-started if unset; oke: https://ant.azrty.com; kw: https://ant.kw.watteel.lab)
#   ANT_TOKEN / ANT_TOKEN_FILE   admin service token, raw or credential JSON (never written to results)
#   ANT_CA_BUNDLE    PEM bundle for a private CA (kw)
#   ANT_VERSION      version label recorded as Ant version (default: git sha)
#   ANT_CONF_STATE   state directory of the throwaway local instance (default /tmp/ant-conformance-dev)
# By default the oke target skips the huggingface format (OKE has no GPUs and Hugging Face is not
# configured there); name it explicitly to run it. local and kw run every format.
# Needs: docker and go (the harness is the Go package conformance/harness, driven by cmd/conformance).
set -eu
HERE=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$HERE/.." && pwd)
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
cd "$ROOT"
set +e
go run ./cmd/conformance "$@"
RC=$?
[ -n "$STARTED" ] && [ -z "${ANT_KEEP_INSTANCE:-}" ] && "$HERE/dev-instance.sh" stop >/dev/null
exit $RC
