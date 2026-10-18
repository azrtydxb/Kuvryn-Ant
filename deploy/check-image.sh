#!/bin/sh
# Permission/entrypoint smoke only; no database or provider environment is started.
set -eu
image=${1:-kuvryn-ant:packaging-check}
platform=${2:-linux/arm64}
docker run --rm --platform "$platform" --read-only --cap-drop ALL \
  --security-opt no-new-privileges \
  --mount type=volume,destination=/var/lib/ant \
  --tmpfs /tmp:rw,noexec,nosuid,size=16m \
  --entrypoint /bin/sh "$image" -ec '
    test "$(id -u)" = 10001
    test -r /etc/ssl/certs/ca-certificates.crt
    if /usr/local/bin/ant-entrypoint invalid-command >/tmp/ant-smoke 2>&1; then exit 1; fi
    grep -q "usage: ant" /tmp/ant-smoke
    test "$(stat -c %a "$ANT_DATA_DIR")" = 700
    test "$(stat -c %u "$ANT_DATA_DIR")" = 10001
    echo "Non-root binary, private persistent directory, CA bundle and read-only root smoke passed."
  '
