#!/bin/sh
# Local rendering checks only: never installs a release or contacts Kubernetes.
set -eu
chart=$(CDPATH= cd -- "$(dirname -- "$0")/chart" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT HUP INT TERM
helm lint "$chart"
helm template fixture "$chart" >"$fixture/disk.yaml"
helm template fixture "$chart" -f "$chart/../examples/oke-minio.yaml" >"$fixture/s3.yaml"
helm template fixture "$chart" --set persistence.existingClaim=existing --set rootKey.previousKeysManifestKey=previous.json --set metrics.headlessService=true --set networkPolicy.enabled=true --set auth.oidc.existingSecret=oidc --set auth.saml.existingSecret=saml --set auth.ldap.existingSecret=ldap >"$fixture/options.yaml"
# Reject configurations that could violate serving identity or writer ownership.
for setting in replicaCount=2 storage.backend=unknown shutdownSeconds=90 terminationGracePeriodSeconds=10 publicURL=http://ant.example.invalid ingress.enabled=true storage.backend=s3; do
	if helm template invalid "$chart" --set "$setting" >/dev/null 2>&1; then
		printf 'Unexpectedly accepted invalid setting: %s\n' "$setting" >&2
		exit 1
	fi
done
need() { # need FILE FIXED_STRING
	grep -qF -- "$2" "$1" || {
		printf 'Missing invariant in %s: %s\n' "$1" "$2" >&2
		exit 1
	}
}
for file in "$fixture/disk.yaml" "$fixture/s3.yaml" "$fixture/options.yaml"; do
	for invariant in 'replicas: 1' 'type: Recreate' 'runAsNonRoot: true' 'runAsUser: 10001' 'readOnlyRootFilesystem: true' '/health/live' '/health/ready' 'persistentVolumeClaim:' 'secretKeyRef:' 'automountServiceAccountToken: false'; do
		need "$file" "$invariant"
	done
	if grep -q '^kind: Secret$' "$file"; then
		echo 'Chart must not render plaintext credentials' >&2
		exit 1
	fi
done
need "$fixture/disk.yaml" 'kind: PersistentVolumeClaim'
need "$fixture/s3.yaml" 'kind: PersistentVolumeClaim' # S3 requires persistent local staging
if grep -q 'kind: PersistentVolumeClaim' "$fixture/options.yaml"; then
	echo 'Existing claim must not render a new PersistentVolumeClaim' >&2
	exit 1
fi
need "$fixture/options.yaml" 'ANT_PREVIOUS_ROOT_KEYS_FILE'
need "$fixture/options.yaml" 'clusterIP: None'
need "$fixture/options.yaml" 'kind: NetworkPolicy'
need "$fixture/s3.yaml" 'ANT_S3_CREDENTIALS_FILE'
need "$fixture/s3.yaml" 'ANT_S3_CA_FILE'
for kind in OIDC SAML LDAP; do
	need "$fixture/options.yaml" "ANT_${kind}_CONFIG_FILE"
done
echo 'Chart disk/S3/auth/policy render checks passed.'
