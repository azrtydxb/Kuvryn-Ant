#!/bin/sh
# Local rendering checks only: never installs a release or contacts Kubernetes.
set -eu
chart=$(CDPATH= cd -- "$(dirname -- "$0")/chart" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
helm lint "$chart"
helm template fixture "$chart" > "$fixture/disk.yaml"
helm template fixture "$chart" -f "$chart/../examples/oke-minio.yaml" > "$fixture/s3.yaml"
helm template fixture "$chart" --set persistence.existingClaim=existing --set rootKey.previousKeysManifestKey=previous.json --set metrics.headlessService=true --set networkPolicy.enabled=true --set auth.oidc.existingSecret=oidc --set auth.saml.existingSecret=saml --set auth.ldap.existingSecret=ldap > "$fixture/options.yaml"
# Reject configurations that could violate serving identity or writer ownership.
for setting in replicaCount=2 storage.backend=unknown shutdownSeconds=90 terminationGracePeriodSeconds=10 publicURL=http://ant.example.invalid ingress.enabled=true storage.backend=s3; do
  if helm template invalid "$chart" --set "$setting" > /dev/null 2>&1; then
    printf 'Unexpectedly accepted invalid setting: %s\n' "$setting" >&2
    exit 1
  fi
done
python3 - "$fixture" <<'PY'
import pathlib, sys
root = pathlib.Path(sys.argv[1])
disk = (root/'disk.yaml').read_text()
s3 = (root/'s3.yaml').read_text()
options = (root/'options.yaml').read_text()
for text in (disk, s3, options):
    for invariant in ('replicas: 1', 'type: Recreate', 'runAsNonRoot: true', 'runAsUser: 10001', 'readOnlyRootFilesystem: true', '/health/live', '/health/ready', 'persistentVolumeClaim:', 'secretKeyRef:', 'automountServiceAccountToken: false'):
        assert invariant in text, invariant
    assert 'kind: Secret\n' not in text, 'Chart must not render plaintext credentials'
assert 'kind: PersistentVolumeClaim' in disk
assert 'kind: PersistentVolumeClaim' in s3, 'S3 requires persistent local staging'
assert 'kind: PersistentVolumeClaim' not in options
assert 'ANT_PREVIOUS_ROOT_KEYS_FILE' in options
assert 'clusterIP: None' in options and 'kind: NetworkPolicy' in options
assert 'ANT_S3_CREDENTIALS_FILE' in s3 and 'ANT_S3_CA_FILE' in s3
for kind in ('OIDC', 'SAML', 'LDAP'):
    assert 'ANT_'+kind+'_CONFIG_FILE' in options
print('Chart disk/S3/auth/policy render checks passed.')
PY
