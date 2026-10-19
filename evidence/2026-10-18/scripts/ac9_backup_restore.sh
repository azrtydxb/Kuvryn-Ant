#!/bin/sh
# AC9 part 1: online backup of the live kw instance, then restore into a SCRATCH database and a SCRATCH blob prefix,
# serve the restored copy from a throwaway pod and verify it. Nothing is written to the production database or prefix.
#
#   ac9_backup_restore.sh <out-dir> <s3poke> <raw-manifest.json>
#
# Needs: kubectl (context kw), jq. Secrets are handled inside the cluster only; nothing secret is printed.
set -eu
OUT=$1
S3POKE=$2
MANIFEST=$3
CTX=kw
NS=kuvryn-ant
K="kubectl --context $CTX -n $NS"
mkdir -p "$OUT"
log() { printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" | tee -a "$OUT/ac9-backup-restore.txt"; }
now() { date +%s; }

log "== scratch resources"
$K apply -f - <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: evr-backup}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: longhorn
  resources: {requests: {storage: 40Gi}}
EOF
PRIMARY=$($K get pod -l cnpg.io/instanceRole=primary -o jsonpath='{.items[0].metadata.name}')
$K exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "DROP DATABASE IF EXISTS ant_scratch" >/dev/null
$K exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "CREATE DATABASE ant_scratch OWNER ant" >/dev/null
log "scratch database ant_scratch created on $PRIMARY"

# Rehearsal pod = the production pod template (same image, env, secrets mounts) with a scratch volume and no server.
$K get deploy ant-ant -o json | jq '{apiVersion:"v1",kind:"Pod",metadata:{name:"evr-rehearsal"},
  spec:(.spec.template.spec
    | .volumes |= (map(if .name=="data" then {name:"data",emptyDir:{}} else . end) + [{name:"backup",persistentVolumeClaim:{claimName:"evr-backup"}}])
    | .containers[0].volumeMounts += [{name:"backup",mountPath:"/backup"}]
    | .containers[0].command=["sleep","86400"]
    | del(.containers[0].readinessProbe,.containers[0].livenessProbe,.containers[0].startupProbe)
    | .restartPolicy="Never")}' >"$OUT/.pod.json"
$K delete pod evr-rehearsal --ignore-not-found --wait=true >/dev/null
$K apply -f "$OUT/.pod.json" >/dev/null
rm -f "$OUT/.pod.json"
$K wait --for=condition=Ready pod/evr-rehearsal --timeout=240s >/dev/null
IMG=$($K get pod evr-rehearsal -o jsonpath='{.spec.containers[0].image}')
log "rehearsal pod ready; image $IMG"

# Production reference numbers taken just before the backup (API, read-only).
TOKEN=$(jq -r .secret "$HOME/.kuvryn-ant/kw-automation-token.txt")
api() { curl -sk -H "Authorization: Bearer $TOKEN" "https://ant.kw.watteel.lab$1"; }
{
	api /api/v1/feeds | jq '{feeds:(.items|length)}'
	api /api/v1/storage/usage | jq '{blob_count,blob_bytes}'
} | jq -s add >"$OUT/prod-reference.json"
log "production reference: $(jq -c . "$OUT/prod-reference.json" | tr '\n' ' ')"

if [ "${SKIP_BACKUP:-0}" = 1 ]; then
	SHA=$(jq -r .manifest_sha256 "$OUT/backup-summary.json")
	log "reusing the existing backup directory, manifest_sha256=$SHA"
else
	log "== backup (online, service keeps running)"
	T0=$(now)
	$K exec evr-rehearsal -- sh -c 'rm -rf /backup/b1; ant backup --concurrency 4 /backup/b1' >"$OUT/backup-summary.json" 2>"$OUT/backup.stderr" || {
		log "BACKUP FAILED"
		tail -5 "$OUT/backup.stderr"
		exit 1
	}
	T1=$(now)
	SHA=$(jq -r .manifest_sha256 "$OUT/backup-summary.json")
	log "backup done in $((T1 - T0))s; manifest_sha256=$SHA"
	jq -c '{schema_version,blob_count,blob_bytes,tables:(.tables|length),manifest_sha256}' "$OUT/backup-summary.json" 2>/dev/null | tee -a "$OUT/ac9-backup-restore.txt" || true
	du_kib=$($K exec evr-rehearsal -- sh -c 'du -sk /backup/b1 | cut -f1')
	log "backup directory size: $((du_kib / 1024)) MiB"

	log "== backup verify (no database or keys needed)"
	T0=$(now)
	$K exec evr-rehearsal -- ant backup verify --expect-manifest-sha256 "$SHA" --concurrency 4 /backup/b1 >"$OUT/backup-verify.json" 2>"$OUT/verify.stderr" && log "verify OK in $(($(now) - T0))s" || log "VERIFY FAILED"

	log "== negative: wrong expected manifest digest must be refused"
	if $K exec evr-rehearsal -- ant backup verify --expect-manifest-sha256 0000000000000000000000000000000000000000000000000000000000000000 /backup/b1 >/dev/null 2>"$OUT/verify-wrong.stderr"; then log "UNEXPECTED: wrong digest accepted"; else log "refused as expected: $(tail -1 "$OUT/verify-wrong.stderr" | cut -c1-160)"; fi

fi
if [ "${WORKAROUND:-0}" = 1 ]; then
	$K exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "ALTER DATABASE ant_scratch SET session_replication_role = replica" >/dev/null
	log "WORKAROUND: ant_scratch has session_replication_role=replica so the replication triggers do not fire during COPY (bug 5)"
fi
log "== restore into scratch database + scratch blob prefix"
RESTORE='export ANT_DATABASE_URL=$(printf %s "$ANT_DATABASE_URL" | sed "s#:5432/ant?#:5432/ant_scratch?#"); export ANT_S3_PREFIX=scratch/; ant restore --expect-manifest-sha256 '"$SHA"' --concurrency 4 /backup/b1'
T0=$(now)
$K exec evr-rehearsal -- sh -c "$RESTORE" >"$OUT/restore-summary.json" 2>"$OUT/restore.stderr" || {
	log "RESTORE FAILED"
	tail -5 "$OUT/restore.stderr"
	exit 1
}
T1=$(now)
log "restore done in $((T1 - T0))s"
jq -c . "$OUT/restore-summary.json" | cut -c1-400 | tee -a "$OUT/ac9-backup-restore.txt"

if [ "${WORKAROUND:-0}" = 1 ]; then
	$K exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "ALTER DATABASE ant_scratch RESET session_replication_role" >/dev/null
	log "workaround removed again before serving (triggers active)"
fi
log "== serve the restored copy (scratch DB + scratch prefix) on 127.0.0.1:8081 inside the pod"
$K exec evr-rehearsal -- sh -c 'export ANT_DATABASE_URL=$(printf %s "$ANT_DATABASE_URL" | sed "s#:5432/ant?#:5432/ant_scratch?#"); export ANT_S3_PREFIX=scratch/ ANT_LISTEN=127.0.0.1:8081 ANT_GC_INTERVAL=0; (setsid ant serve >/tmp/scratch-serve.log 2>&1 &); sleep 1'
i=0
until $K exec evr-rehearsal -- wget -q -O - http://127.0.0.1:8081/health/ready >/dev/null 2>&1; do
	i=$((i + 1))
	[ $i -gt 60 ] && {
		log "restored server did not become ready"
		$K exec evr-rehearsal -- tail -5 /tmp/scratch-serve.log | cut -c1-300
		exit 1
	}
	sleep 2
done
log "restored server ready after ~$((i * 2))s"

# Verification through the restored server, with the production token (token hashes are part of the backup).
# status + feed count + storage usage
{
	$K exec evr-rehearsal -- sh -c "wget -q -O - --header 'Authorization: Bearer $TOKEN' http://127.0.0.1:8081/api/v1/feeds" | jq '{feeds:(.items|length)}'
	$K exec evr-rehearsal -- sh -c "wget -q -O - --header 'Authorization: Bearer $TOKEN' http://127.0.0.1:8081/api/v1/storage/usage" | jq '{blob_count,blob_bytes}'
} | jq -s add >"$OUT/restored-reference.json"
log "restored reference: $(jq -c . "$OUT/restored-reference.json" | tr '\n' ' ')"
log "production reference was: $(jq -c . "$OUT/prod-reference.json" | tr '\n' ' ') (restored may differ by objects created after the snapshot)"

# digest-verify the replicated raw feed through the restored server: antbench inside the pod, hashes compared with the publisher manifest
$K cp ../../.bench-bin/antbench evr-rehearsal:/tmp/antbench 2>/dev/null || $K cp .bench-bin/antbench evr-rehearsal:/tmp/antbench
$K exec evr-rehearsal -- chmod +x /tmp/antbench
jq -r 'keys[]' "$MANIFEST" | sed 's#^#http://127.0.0.1:8081/feeds/ev5s-raw-replica/raw/#' >"$OUT/.urls.txt"
$K cp "$OUT/.urls.txt" evr-rehearsal:/tmp/urls.txt
$K exec evr-rehearsal -- sh -c "echo '{\"secret\":\"$TOKEN\"}' >/tmp/tok.json; /tmp/antbench get -token-file /tmp/tok.json -urls-file /tmp/urls.txt -n 350 -c 1 -sequential -hash" >"$OUT/restored-raw-hashes.json"
rm -f "$OUT/.urls.txt"
jq -r 'to_entries[].value.sha256' "$MANIFEST" | sort >"$OUT/.want"
jq -r '.result.body_sha256_counts|keys[]' "$OUT/restored-raw-hashes.json" | sort >"$OUT/.got"
if diff -q "$OUT/.want" "$OUT/.got" >/dev/null; then log "restored raw feed: all $(wc -l <"$OUT/.want" | tr -d ' ') replicated artifacts digest-identical to the publisher manifest (status $(jq -c .result.status "$OUT/restored-raw-hashes.json"))"; else log "restored raw feed: DIGEST SET DIFFERS"; fi
rm -f "$OUT/.want" "$OUT/.got"

log "== diagnostics of the restored instance"
$K exec evr-rehearsal -- sh -c 'export ANT_DATABASE_URL=$(printf %s "$ANT_DATABASE_URL" | sed "s#:5432/ant?#:5432/ant_scratch?#"); export ANT_S3_PREFIX=scratch/; ant diagnostics' 2>/dev/null | jq -c 'del(.. | .dsn? , .database_url?)' 2>/dev/null | cut -c1-600 | tee -a "$OUT/ac9-backup-restore.txt" || true

log "== cleanup"
$K exec evr-rehearsal -- sh -c 'pkill ant || true' >/dev/null 2>&1 || true
$K delete pod evr-rehearsal --wait=true >/dev/null
$K delete pvc evr-backup --wait=false >/dev/null
$K exec "$PRIMARY" -c postgres -- psql -U postgres -tAc "DROP DATABASE ant_scratch WITH (FORCE)" >/dev/null
"$S3POKE" kw rmprefix scratch/ | tail -1 | tee -a "$OUT/ac9-backup-restore.txt"
log "done"
