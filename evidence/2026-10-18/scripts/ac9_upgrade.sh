#!/bin/sh
# AC9 part 2: Helm upgrade rehearsal from the previous image to the current one, in a SCRATCH namespace (ant-upg) on
# kw with its own scratch database (ant_upg) and blob prefix (upg/). Production is not touched.
#
#   ac9_upgrade.sh <out-dir> <chart-dir> <prev-tag> <new-tag> <crane-binary-dir-or-empty>
#
# Needs: kubectl (context kw), helm, jq, curl, crane. Credentials live in a mode-0700 temp dir and are never printed.
set -eu
OUT=$1
CHART=$2
PREV=$3
NEW=$4
CTX=kw
NS=ant-upg
K="kubectl --context $CTX -n $NS"
KP="kubectl --context $CTX -n kuvryn-ant"
TMP=$(mktemp -d)
chmod 700 "$TMP"
mkdir -p "$OUT"
LOG=$OUT/ac9-upgrade.txt
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$LOG"; }
trap 'rm -rf "$TMP"' EXIT
REPO=192.168.10.131/azrtydxb/kuvryn-ant
migr() { $K exec deploy/ant-ant -- sh -c "ant diagnostics -o /tmp/d.tgz >/dev/null && tar xzOf /tmp/d.tgz ant-diagnostics/migrations.json" | jq -c "{applied_version,binary_version,pending,database_newer_than_binary}"; }

log "== scratch namespace, secrets copied from production (same root key, scratch DSN and S3 prefix)"
kubectl --context $CTX create namespace $NS --dry-run=client -o yaml | kubectl --context $CTX apply -f - >/dev/null
for s in ant-root-key ant-pg-rootcert nexus-pull ant-s3; do
	$KP get secret $s -o json | jq 'del(.metadata.namespace,.metadata.uid,.metadata.resourceVersion,.metadata.creationTimestamp,.metadata.ownerReferences,.metadata.annotations,.metadata.managedFields)' | $K apply -f - >/dev/null
done
dsn() { $KP get secret ant-postgres -o jsonpath='{.data.dsn}' | base64 -d | sed "s#:5432/ant?#:5432/$1?#"; }
$K delete secret ant-postgres --ignore-not-found >/dev/null
$K create secret generic ant-postgres --from-literal=dsn="$(dsn ant_upg)" >/dev/null
PRIMARY=$($KP get pod -l cnpg.io/instanceRole=primary -o jsonpath='{.items[0].metadata.name}')
psqlx() { $KP exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "$1" >/dev/null; }
psqlx "DROP DATABASE IF EXISTS ant_upg WITH (FORCE)"
psqlx "CREATE DATABASE ant_upg OWNER ant"

cat >"$TMP/values.yaml" <<EOF
image: {repository: $REPO, tag: "$PREV", pullPolicy: IfNotPresent}
imagePullSecrets: [{name: nexus-pull}]
publicURL: https://ant-upg.kw.watteel.lab
persistence: {size: 5Gi, storageClass: longhorn}
storage:
  backend: s3
  s3: {endpoint: "http://192.168.10.253:9000", region: us-east-1, bucket: kuvryn-ant, prefix: "upg/", pathStyle: true, credentialsExistingSecret: ant-s3, allowInsecureHTTP: true}
postgres: {existingSecret: ant-postgres, tlsExistingSecret: ant-pg-rootcert}
rootKey: {existingSecret: ant-root-key}
ingress: {enabled: false}
EOF

log "== install previous image $PREV"
T0=$(date +%s)
helm --kube-context $CTX -n $NS upgrade --install ant "$CHART" -f "$TMP/values.yaml" --wait --timeout 10m >"$OUT/helm-install.txt" 2>&1 && log "install ok in $(($(date +%s) - T0))s" || {
	log "INSTALL FAILED"
	tail -5 "$OUT/helm-install.txt"
	exit 1
}
log "running image: $($K get deploy ant-ant -o jsonpath='{.spec.template.spec.containers[0].image}')"
log "migrations at previous image: $(migr)"

log "== bootstrap admin and seed data through the previous image"
$K exec deploy/ant-ant -- ant bootstrap upg >"$TMP/cred.json" 2>"$TMP/boot.err" || $K exec deploy/ant-ant -- ant bootstrap-token >"$TMP/cred.json" 2>>"$TMP/boot.err" || true
log "bootstrap credential fields: $(jq -c keys "$TMP/cred.json" 2>/dev/null) stderr: $(tail -1 "$TMP/boot.err" | cut -c1-160)"
TOK=$(jq -r '.token // .secret' "$TMP/cred.json")
# Observed (bug 6): on a fresh database the data-plane routes (/feeds/..., /v2/) answer "404 page not found" until the
# pod is restarted once after bootstrap; the management API works immediately.
$K rollout restart deploy/ant-ant >/dev/null
$K rollout status deploy/ant-ant --timeout=180s >/dev/null
$K port-forward svc/ant-ant 18080:8080 >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null; rm -rf "$TMP"' EXIT
sleep 4
log "port-forward health: $(curl -s -o /dev/null -w %{http_code} http://127.0.0.1:18080/health/ready) token length: ${#TOK}"
B=http://127.0.0.1:18080
go build -o "$TMP/ac9seed" ./evidence/2026-10-18/tools/ac9seed
"$TMP/ac9seed" seed "$B" "$TMP/cred.json" "$TMP/manifest.json" | tee -a "$LOG"
mkdir -p "$TMP/dc"
printf '{"auths":{"127.0.0.1:18080":{"auth":"%s"}}}' "$(printf 'service:%s' "$TOK" | base64 | tr -d '\n')" >"$TMP/dc/config.json"
head -c 2000000 /dev/urandom >"$TMP/blob.bin"
(cd "$TMP" && tar cf layer.tar blob.bin)
DOCKER_CONFIG=$TMP/dc crane append --insecure --oci-empty-base -f "$TMP/layer.tar" -t 127.0.0.1:18080/upg-oci/demo:v1 >/dev/null 2>&1 && log "pushed OCI image upg-oci/demo:v1" || log "OCI push failed"

log "== pre-upgrade backup (procedure step 2) and verify"
T0=$(date +%s)
$K exec deploy/ant-ant -- ant backup /var/lib/ant/data/pre-upgrade >"$OUT/pre-upgrade-backup.json" 2>/dev/null
SHA=$(jq -r .manifest_sha256 "$OUT/pre-upgrade-backup.json")
log "pre-upgrade backup in $(($(date +%s) - T0))s manifest_sha256=$SHA"
$K exec deploy/ant-ant -- ant backup verify --expect-manifest-sha256 "$SHA" /var/lib/ant/data/pre-upgrade >/dev/null 2>&1 && log "backup verify OK" || log "backup verify FAILED"

log "== availability poller (in-cluster, 0.5 s) during the upgrade"
$K delete pod evu-poll --ignore-not-found --wait=true >/dev/null
$K run evu-poll --image=busybox:1.36 --restart=Never --command -- sh -c 'while true; do if wget -q -T 1 -O /dev/null http://ant-ant.ant-upg.svc:8080/health/ready; then echo "$(date +%s) up"; else echo "$(date +%s) down"; fi; sleep 0.5; done' >/dev/null
$K wait --for=condition=Ready pod/evu-poll --timeout=120s >/dev/null
sleep 5

log "== helm upgrade $PREV -> $NEW"
T0=$(date +%s)
helm --kube-context $CTX -n $NS upgrade ant "$CHART" -f "$TMP/values.yaml" --set image.tag="$NEW" --wait --timeout 10m >"$OUT/helm-upgrade.txt" 2>&1 && log "upgrade ok; helm --wait returned after $(($(date +%s) - T0))s" || {
	log "UPGRADE FAILED"
	tail -5 "$OUT/helm-upgrade.txt"
}
sleep 5
$K logs evu-poll >"$OUT/availability-poll.txt" 2>/dev/null || true
UP=$(grep -c ' up$' "$OUT/availability-poll.txt" || true)
DOWN=$(grep -c ' down$' "$OUT/availability-poll.txt" || true)
FIRST=$(grep ' down$' "$OUT/availability-poll.txt" | head -1 | cut -d' ' -f1)
LAST=$(grep ' down$' "$OUT/availability-poll.txt" | tail -1 | cut -d' ' -f1)
log "availability: $UP up samples, $DOWN down samples; first down $FIRST last down $LAST => observed outage ~$((LAST - FIRST + 1))s (Recreate strategy; single replica)"
$K delete pod evu-poll --wait=false >/dev/null
log "running image after upgrade: $($K get deploy ant-ant -o jsonpath='{.spec.template.spec.containers[0].image}')"
log "migrations after upgrade: $(migr)"
$K logs deploy/ant-ant 2>/dev/null | grep -i -E 'migrat' | head -5 | cut -c1-300 | tee -a "$LOG" || true

log "== post-upgrade verification"
kill $PF 2>/dev/null || true
$K port-forward svc/ant-ant 18080:8080 >/dev/null 2>&1 &
PF=$!
sleep 4
log "port-forward health: $(curl -s -o /dev/null -w %{http_code} http://127.0.0.1:18080/health/ready) token length: ${#TOK}"
log "seeded raw objects re-read after upgrade: $("$TMP/ac9seed" verify "$B" "$TMP/cred.json" "$TMP/manifest.json")"
DOCKER_CONFIG=$TMP/dc crane validate --insecure --remote 127.0.0.1:18080/upg-oci/demo:v1 >/dev/null 2>&1 && log "OCI image validates after upgrade" || log "OCI validate FAILED"
log "publish after upgrade: http $("$TMP/ac9seed" put "$B" "$TMP/cred.json" post/new.bin)"
FID=$(curl -s -H "Authorization: Bearer $TOK" "$B/api/v1/feeds" | jq -r '.items[]|select(.slug=="upg-raw")|.id')
code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $TOK" "$B/api/v1/feeds/$FID/delete-policy")
log "new-in-this-release endpoint (delete-policy, migration 025) answers: http $code"

log "== rollback rehearsal 1: previous image against the upgraded schema must refuse to start"
kill $PF 2>/dev/null || true
helm --kube-context $CTX -n $NS upgrade ant "$CHART" -f "$TMP/values.yaml" --set image.tag="$PREV" --wait --timeout 100s >"$OUT/helm-rollback-image.txt" 2>&1 && log "UNEXPECTED: previous image started on newer schema" || log "previous image did not become ready (expected)"
sleep 5
$K logs deploy/ant-ant --tail=5 2>/dev/null | cut -c1-300 | tee -a "$LOG" || true

log "== rollback rehearsal 2: documented path = restore the pre-upgrade backup into a fresh database, previous image"
$K scale deploy/ant-ant --replicas=0 >/dev/null
psqlx "DROP DATABASE IF EXISTS ant_upg_rb WITH (FORCE)"
psqlx "CREATE DATABASE ant_upg_rb OWNER ant"
# Workaround for bug 5 (restore fires the replication triggers on COPY): disable them for the restore only.
psqlx "ALTER DATABASE ant_upg_rb SET session_replication_role = replica"
$K delete secret ant-postgres >/dev/null
$K create secret generic ant-postgres --from-literal=dsn="$(dsn ant_upg_rb)" >/dev/null
# the backup lives on the data PVC: mount it in a one-off pod with the previous image
$K get deploy ant-ant -o json | jq --arg img "$REPO:$PREV" '{apiVersion:"v1",kind:"Pod",metadata:{name:"evu-rb"},spec:(.spec.template.spec | .containers[0].image=$img | .containers[0].command=["sleep","3600"] | .containers[0].env |= map(if .name=="ANT_S3_PREFIX" then .value="upg-rb/" else . end) | del(.containers[0].readinessProbe,.containers[0].livenessProbe,.containers[0].startupProbe) | .restartPolicy="Never")}' | $K apply -f - >/dev/null
$K wait --for=condition=Ready pod/evu-rb --timeout=180s >/dev/null
T0=$(date +%s)
$K exec evu-rb -- ant restore --expect-manifest-sha256 "$SHA" /var/lib/ant/data/pre-upgrade >"$OUT/rollback-restore.json" 2>"$OUT/rollback-restore.stderr" && log "restore of the pre-upgrade backup with $PREV OK in $(($(date +%s) - T0))s" || log "ROLLBACK RESTORE FAILED: $(tail -1 "$OUT/rollback-restore.stderr" | cut -c1-200)"
psqlx "ALTER DATABASE ant_upg_rb RESET session_replication_role"
$K exec evu-rb -- sh -c 'export ANT_LISTEN=127.0.0.1:8081 ANT_GC_INTERVAL=0; (setsid ant serve >/tmp/rb.log 2>&1 &); sleep 8; wget -q -O - http://127.0.0.1:8081/health/ready' >/dev/null 2>&1 && log "previous image serves the restored pre-upgrade state" || log "restored pre-upgrade state did not serve"
$K port-forward pod/evu-rb 18081:8081 >/dev/null 2>&1 &
PF2=$!
sleep 4
log "rollback copy: $("$TMP/ac9seed" verify http://127.0.0.1:18081 "$TMP/cred.json" "$TMP/manifest.json"); post-upgrade write read back: http $("$TMP/ac9seed" get http://127.0.0.1:18081 "$TMP/cred.json" post/new.bin) (404 = correctly absent)"
kill $PF2 2>/dev/null || true

log "== cleanup"
$K delete pod evu-rb --wait=false >/dev/null || true
helm --kube-context $CTX -n $NS uninstall ant >/dev/null 2>&1 || true
kubectl --context $CTX delete namespace $NS --wait=false >/dev/null
psqlx "DROP DATABASE IF EXISTS ant_upg WITH (FORCE)"
psqlx "DROP DATABASE IF EXISTS ant_upg_rb WITH (FORCE)"
log "done (scratch blob prefixes upg/ and upg-rb/ removed separately with s3poke rmprefix)"
