# Terraform provider and module registry adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/terraform/` for feeds of format `terraform`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/terraform.md`.

```go
repo, err := terraform.New(db, service)
err = repo.SetPublicURL("https://tf.example.com") // HTTPS or literal loopback
```

The feed must have format `terraform`. All routes except discovery need a token
(Bearer, or Basic with the token as password), verified by the shared
client-authentication layer; browser principals are discarded. Terraform sends it from
a `credentials "host"` block. Ingestion requires feed Admin plus Publish on the pinned
local source.

## Client setup (feed-scoped mount)

Terraform fetches `https://HOST/.well-known/terraform.json` at the host root, which
lies outside the feed mount. Give each feed its own hostname and have the reverse
proxy map `/.well-known/terraform.json` to
`/feeds/{slug}/terraform/.well-known/terraform.json` (served without credentials).
The document returns absolute URLs under the configured public URL:
`modules.v1` = `.../feeds/{slug}/terraform/v1/modules/`, `providers.v1` likewise.
Then use `source = "HOST/ns/name/system"` (modules) or `HOST/ns/type` (providers).

## Routes (under `/feeds/{slug}/terraform/`)

- `GET v1/modules/{ns}/{name}/{system}/versions`
- `GET v1/modules/{ns}/{name}/{system}/{version}/download` -> 204 + `X-Terraform-Get`
- `GET|HEAD .../{version}/archive.tar.gz` immutable archive
- `PUT v1/modules/{ns}/{name}/{system}/{version}` body = tar.gz (optional `X-Checksum-Sha256`)
- `GET v1/providers/{ns}/{type}/versions` (versions, protocols, platforms)
- `GET v1/providers/{ns}/{type}/{version}/download/{os}/{arch}` (download_url, shasums_url,
  shasums_signature_url, shasum, signing_keys)
- `GET|HEAD v1/providers/{ns}/{type}/{version}/files/{filename}` zip, `_SHA256SUMS`, `.sig`
- `PUT v1/providers/{ns}/{type}/{version}` multipart/form-data: fields `shasums`,
  `shasums_sig`, `signing_key` (ASCII-armored single public key), optional `protocols`
  (default `5.0`), and one `zip` part per platform whose filename is
  `terraform-provider-{type}_{version}_{os}_{arch}.zip`.

Names are lowercase (`[a-z0-9_-]`, provider type `[a-z0-9-]`), versions semver without
build metadata. `DELETE v1/providers/{ns}/{type}/{version}` and
`DELETE v1/modules/{ns}/{name}/{system}/{version}` delete a version through the core
(gated by the feed's delete policy); other methods return 405. Versions are immutable.

## Validation and atomicity

Providers: the detached signature must verify over SHA256SUMS with the supplied key
(armored signatures are normalized to binary); SHA256SUMS must list exactly the uploaded
zips with matching SHA-256; each zip must hold `terraform-provider-{type}[_v...]` at its
root, with safe names, no symlinks and bounded size; secret keys are rejected. Modules:
bounded gzip+tar, safe names, no hardlinks/devices/escaping symlinks, a root `.tf` or
`.tf.json`. Nothing is extracted or executed. A provider version (max 12 platforms,
bundle cap 16 assets) publishes through `PublishBundle` atomically; the `metadata.json`
marker means only complete versions are listed. On every resolution the zip digest,
SHA256SUMS line, signature and key id are re-verified; mismatch returns 502, never bytes.
Identical retries return 201; different bytes for an existing version return 409.

## Known limitations

- Unit tests use a client-style flow; the real Terraform CLI (`terraform init` with a signed provider and a module, and pull-through of registry.terraform.io) is exercised by `conformance/run.sh local terraform`, which needs a TLS host.
- Terraform sends no credentials when fetching `download_url`, `shasums_url` and
  `shasums_signature_url`. The download document therefore advertises those three URLs
  with an `ant-sig` query parameter: an HMAC (key derived from the root key) over the
  exact path and a 15 minute expiry, carrying the principal that was authorized to read
  the provider. The principal is re-resolved on use, so revoked tokens and removed grants
  stop working immediately. Module archive URLs are not signed.
- Upstream signature files named `SHA256SUMS.<keyid>.sig` (HashiCorp) are accepted and
  re-served as `SHA256SUMS.sig`.
- Module closure checks cover archive structure only, not `source = "./..."` references.
- Remote sources (pull-through): `versions` and the per-platform `download` document are read through the metadata cache; the feed's `download` answer keeps `shasum` and `signing_keys` exactly as the registry sent them and points the file URLs at the feed, so Terraform verifies the zip, SHA256SUMS and signature itself against the upstream publisher key. Ant also checks what it caches (the zip against `shasum`, SHA256SUMS against the zip line, the detached signature when it parses as a single public key). Release files normally live on another host (releases.hashicorp.com): add it to the source's redirect origins. Module `download` resolves the module detail document (`source` GitHub repository and `tag`) to the GitHub tarball (`codeload.github.com`), cached once as a bounded, safe tar.gz; registries whose modules are not GitHub repositories, and zip archives, are not supported.
