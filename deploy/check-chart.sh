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
helm template fixture "$chart" --set publicURL=https://ant.example.com --set ingress.enabled=true --set ingress.host=ant.example.com --set ingress.tlsSecret=ant-tls >"$fixture/ingress.yaml"
helm template fixture "$chart" --set publicURL=https://ant.example.com --set httpRoute.enabled=true --set 'httpRoute.hostnames={ant.example.com}' --set 'httpRoute.parentRefs[0].name=edge' >"$fixture/httproute.yaml"
helm template fixture "$chart" --set upstreamCA.existingSecret=upstream-ca >"$fixture/ca-secret.yaml"
helm template fixture "$chart" --set upstreamCA.existingConfigMap=upstream-ca --set upstreamCA.mapKey=bundles.json >"$fixture/ca-configmap.yaml"
# Reject configurations that could violate serving identity or writer ownership.
for setting in replicaCount=2 storage.backend=unknown shutdownSeconds=90 terminationGracePeriodSeconds=10 publicURL=http://ant.example.invalid ingress.enabled=true storage.backend=s3 upstreamCA.existingSecret=a,upstreamCA.existingConfigMap=b upstreamCA.existingSecret=a,upstreamCA.mapKey=../x upstreamCA.unknown=1; do
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
# The MCP endpoint is routed by both the Ingress and the HTTPRoute.
need "$fixture/ingress.yaml" 'path: /mcp'
need "$fixture/httproute.yaml" 'value: /mcp'
# Operator CA map: mounted from a Secret or a ConfigMap and passed by env.
need "$fixture/ca-secret.yaml" 'ANT_UPSTREAM_CA_MAP_FILE'
need "$fixture/ca-secret.yaml" '/run/ant/upstream-ca/ca-map.json'
need "$fixture/ca-secret.yaml" 'secretName: "upstream-ca"'
need "$fixture/ca-configmap.yaml" '/run/ant/upstream-ca/bundles.json'
need "$fixture/ca-configmap.yaml" 'configMap:'
if grep -q ANT_UPSTREAM_CA_MAP_FILE "$fixture/disk.yaml"; then
  echo 'ANT_UPSTREAM_CA_MAP_FILE must not be set without upstreamCA' >&2
  exit 1
fi
for kind in OIDC SAML LDAP; do
  need "$fixture/options.yaml" "ANT_${kind}_CONFIG_FILE"
done
echo 'Chart disk/S3/auth/policy render checks passed.'
