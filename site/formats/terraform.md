# Terraform / OpenTofu

The Terraform adapter implements the module registry protocol (`modules.v1`) and the provider registry protocol (`providers.v1`). `terraform init` and `tofu init` can install private providers and modules from a feed, and a feed can pull through `registry.terraform.io`.

Feed base URL: `https://ant.example.com/feeds/<slug>/terraform` (feed format `terraform`). The service discovery document for the feed is at `https://ant.example.com/feeds/<slug>/terraform/.well-known/terraform.json`.

## Support matrix

| Publish                                                                          | Consume                                         | Remote (pull-through)                                                      | Replica                       | Notes                                                                                           |
| -------------------------------------------------------------------------------- | ----------------------------------------------- | -------------------------------------------------------------------------- | ----------------------------- | ----------------------------------------------------------------------------------------------- |
| Admin ingestion: `PUT` a signed provider release or a module tar.gz (feed admin) | Yes: `terraform init` for modules and providers | Yes: `registry.terraform.io` providers, and modules whose source is GitHub | Yes, through core replication | No provider network mirror protocol. Discovery needs a `host` block or a reverse-proxy mapping. |

Routes under `/feeds/<slug>/terraform/`:

| Method        | Path                                                                              | Purpose                                                                                        |
| ------------- | --------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------- |
| `GET`         | `.well-known/terraform.json`                                                      | Service discovery (no credentials needed). Returns absolute `modules.v1` / `providers.v1` URLs |
| `GET`         | `v1/modules/<ns>/<name>/<system>/versions`                                        | Module versions                                                                                |
| `GET`         | `v1/modules/<ns>/<name>/<system>/<version>/download`                              | `204` with `X-Terraform-Get` pointing at the archive                                           |
| `GET`, `HEAD` | `v1/modules/<ns>/<name>/<system>/<version>/archive.tar.gz`                        | Module archive                                                                                 |
| `PUT`         | `v1/modules/<ns>/<name>/<system>/<version>`                                       | Upload a module (body: tar.gz, optional `X-Checksum-Sha256`)                                   |
| `GET`         | `v1/providers/<ns>/<type>/versions`                                               | Provider versions, protocols, platforms                                                        |
| `GET`         | `v1/providers/<ns>/<type>/<version>/download/<os>/<arch>`                         | `download_url`, `shasums_url`, `shasums_signature_url`, `shasum`, `signing_keys`               |
| `GET`, `HEAD` | `v1/providers/<ns>/<type>/<version>/files/<file>`                                 | Provider zip, `SHA256SUMS`, `SHA256SUMS.sig`                                                   |
| `PUT`         | `v1/providers/<ns>/<type>/<version>`                                              | Upload a provider release (multipart, see below)                                               |
| `DELETE`      | `v1/providers/<ns>/<type>/<version>`, `v1/modules/<ns>/<name>/<system>/<version>` | Delete a version. The artifact core's delete policy controls whether this is allowed.          |

## Limits

- Terraform has no publish protocol. Uploads are adapter-specific `PUT`s and need **Admin** on the feed, plus publish on the selected local source.
- Names are lowercase (`[a-z0-9_-]`; a provider type is `[a-z0-9-]`). Versions are semver without build metadata. A feed lists at most 512 versions per module or provider.
- Provider uploads: at most 12 platforms per version, each zip at most 512 MiB. The detached signature must verify over `SHA256SUMS` with the supplied ASCII-armored public key (secret keys are rejected). `SHA256SUMS` must list exactly the uploaded zips. Each zip must contain `terraform-provider-<type>[_v...]` at its root, with no symlinks. On every resolution, the zip digest, the `SHA256SUMS` line, the signature and the key id are checked again. A mismatch returns 502, never bytes.
- Module uploads: at most 128 MiB compressed and 512 MiB expanded. The archive must have safe names (no hardlinks, devices or escaping symlinks) and a root `.tf` or `.tf.json` file. `source = "./..."` references inside the module are not checked.
- Versions are immutable. An identical retry gets 201, different bytes 409.
- **No provider network mirror protocol** (`provider_installation { network_mirror { ... } }`). Providers are installed through the registry protocol (`direct`).
- Terraform sends no credentials when it fetches `download_url`, `shasums_url` and `shasums_signature_url`. The adapter therefore signs those three URLs with an `ant-sig` query parameter: an HMAC derived from the root key over the exact path, valid for 15 minutes and bound to the principal that was authorized to read the provider. The principal is checked again on use, so a revoked token stops working immediately.
- Module archive URLs (`X-Terraform-Get`) are **not** signed. Terraform downloads them with the credentials from `~/.netrc`.
- `ANT_PUBLIC_URL` must be set: discovery, `X-Terraform-Get` and the provider file URLs are absolute.

::: warning Gap
The recorded conformance run (Terraform 1.16.6, 2026-10-19) passed module installation, but provider installation failed with `401` on the `SHA256SUMS` download. Signed provider file URLs were added after that run and are covered by integration tests. No later real-client run of provider installation (local or pull-through) is recorded. OpenTofu has not been run against Ant at all.
:::

## Service discovery

Terraform reads `https://<host>/.well-known/terraform.json` at the **host root**. Ant serves that document per feed, under the feed path, so you have to connect the two. Pick one of these options:

::: code-group

```hcl [CLI config host block (per user)]
# ~/.terraformrc (Linux/macOS), %APPDATA%/terraform.rc (Windows), or TF_CLI_CONFIG_FILE
host "ant.example.com" {
  services = {
    "modules.v1"   = "https://ant.example.com/feeds/terraform/terraform/v1/modules/"
    "providers.v1" = "https://ant.example.com/feeds/terraform/terraform/v1/providers/"
  }
}

credentials "ant.example.com" {
  token = "<TOKEN>"
}
```

```text [reverse proxy (per hostname)]
Give the feed its own hostname, for example tf.example.com, and map
  GET https://tf.example.com/.well-known/terraform.json
to
  https://ant.example.com/feeds/terraform/terraform/.well-known/terraform.json
The document is served without credentials and contains absolute URLs
under ANT_PUBLIC_URL. Clients then use tf.example.com in source addresses
and in their credentials block.
```

:::

A `host` block overrides discovery for that hostname, and the `credentials` block with the same label supplies the token. The conformance harness uses this form. It also uses a second, made-up hostname label (`reg.example`) to reach another feed on the same server, which shows that one host label per feed works without DNS for the label.

OpenTofu reads the same CLI configuration format (`~/.tofurc`, or `TF_CLI_CONFIG_FILE`) and supports `host` and `credentials` blocks. This has not been tested with Ant.

## Create the feed

In the console, open **Feeds** and click **Create feed** with format `terraform`. On **Sources**, use **Create source** to create a `local` source, attach it with **Attach source** and make it the **Publish policy** default. For a proxy, add a `remote` source with origin `https://registry.terraform.io` and the redirect origins below.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: tf-local-1' \
  -d '{"name":"terraform-local","kind":"local","format":"terraform"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: tf-feed-1' -d @- <<JSON
{"slug":"terraform","format":"terraform","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

See [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin is the **registry root**. `v1/modules/...` and `v1/providers/...` map one to one onto it.

- **Providers.** `versions` and the per-platform `download` document go through the metadata cache and are validated. The feed's answer keeps `shasum` and `signing_keys` exactly as the registry sent them, and points `download_url`, `shasums_url` and `shasums_signature_url` at the feed's `files/<name>` routes. Terraform therefore verifies the zip, `SHA256SUMS` and the signature against the publisher's key (for example HashiCorp's) itself. Ant also checks what it caches: the zip against `shasum`, `SHA256SUMS` against the zip line, and the signature against the supplied key when that parses as a single public key. Upstream `SHA256SUMS.<keyid>.sig` files are served back as `SHA256SUMS.sig`. Release files live on `releases.hashicorp.com`.
- **Modules.** The registry answers `download` with a 204 and an `X-Terraform-Get` header, which the remote runtime does not expose. The feed therefore reads the module detail document (`source` GitHub repository and `tag`), fetches the GitHub tarball from `codeload.github.com`, caches it once (checked as a bounded, safe tar.gz, since no digest is published), and answers `download` with its own archive URL plus the `//*` subdirectory that unwraps GitHub's top-level directory. Modules that are not GitHub repositories, other git hosts and zip archives are not supported.
- Cached provider files and module versions stay available during an outage. Provider `versions` and `download` documents rely on the metadata cache (stale-if-error) and have no store-based offline listing.
- A provider or module owned by a local source is never answered from upstream.

| Upstream                       | `origin`                        | `redirect_origins`                                                                                                                                                            |
| ------------------------------ | ------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Terraform Registry             | `https://registry.terraform.io` | `https://releases.hashicorp.com`, `https://codeload.github.com` (modules)                                                                                                     |
| OpenTofu Registry (not tested) | `https://registry.opentofu.org` | the hosts serving its release downloads (GitHub release assets: `https://github.com`, `https://objects.githubusercontent.com`), and `https://codeload.github.com` for modules |

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: tf-remote-1' \
  -d '{"name":"terraform-registry","kind":"remote","format":"terraform"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: tf-remote-cfg-1' -d '{
  "origin":"https://registry.terraform.io",
  "redirect_origins":["https://releases.hashicorp.com","https://codeload.github.com"],
  "allow_private":false,"timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: tf-remote-feed-1' \
  -d "{\"slug\":\"tf-registry\",\"format\":\"terraform\",\"source_ids\":[\"$REM\"]}"
```

::: warning Unverified
The OpenTofu row is an inference: the repository documents and tests only `registry.terraform.io`. Whether OpenTofu's provider download URLs and module detail documents fit the adapter's assumptions (module `source` on GitHub) has not been checked.
:::

## Client configuration

The adapter accepts `Authorization: Bearer <token>` (from the `credentials` block or `TF_TOKEN_*`) or HTTP Basic with the token as password. Unauthenticated requests get `401`.

### Credentials

::: code-group

```hcl [~/.terraformrc]
credentials "ant.example.com" {
  token = "<TOKEN>"
}
```

```sh [environment]
# dots become underscores; works for Terraform >= 1.2 and OpenTofu
export TF_TOKEN_ant_example_com="$ANT_TOKEN"
```

```sh [~/.netrc (module archives)]
cat >> ~/.netrc <<EOF
machine ant.example.com login token password $ANT_TOKEN
EOF
chmod 600 ~/.netrc
```

:::

Module downloads need the `.netrc` entry, because the archive URL is fetched without the `credentials` block. Provider files use signed URLs and need no extra configuration.

### Consume

With the `host` block from [Service discovery](#service-discovery):

```hcl
terraform {
  required_providers {
    mytype = {
      source  = "ant.example.com/myorg/mytype"
      version = "~> 1.0"
    }
  }
}

module "vpc" {
  source  = "ant.example.com/myorg/vpc/aws"
  version = "1.0.0"
}
```

```sh
terraform init      # or: tofu init
```

For the pull-through feed, give it its own host label (for example `registry.ant.example.com`) in a second `host` and `credentials` block, then use `source = "registry.ant.example.com/hashicorp/random"`.

### Publish a module

```sh
tar -czf vpc-1.0.0.tar.gz -C ./modules/vpc .
curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -H "X-Checksum-Sha256: $(sha256sum vpc-1.0.0.tar.gz | cut -d' ' -f1)" \
  --data-binary @vpc-1.0.0.tar.gz \
  https://ant.example.com/feeds/terraform/terraform/v1/modules/myorg/vpc/aws/1.0.0
```

### Publish a provider

Build the release the way GoReleaser or HashiCorp's release tooling does: one zip per platform named `terraform-provider-<type>_<version>_<os>_<arch>.zip`, a `SHA256SUMS` file listing exactly those zips, and a detached GPG signature over it.

```sh
V=1.0.0; T=mytype
sha256sum terraform-provider-${T}_${V}_*.zip > SHA256SUMS
gpg --detach-sign SHA256SUMS                      # writes SHA256SUMS.sig
gpg --armor --export user@example.com > key.asc   # public key only

curl -fsS -u "token:$ANT_TOKEN" -X PUT \
  -F "shasums=@SHA256SUMS" \
  -F "shasums_sig=@SHA256SUMS.sig" \
  -F "signing_key=<key.asc" \
  -F "protocols=5.0" \
  -F "zip=@terraform-provider-${T}_${V}_linux_amd64.zip" \
  -F "zip=@terraform-provider-${T}_${V}_linux_arm64.zip" \
  https://ant.example.com/feeds/terraform/terraform/v1/providers/myorg/${T}/${V}
```

`protocols` is optional and defaults to `5.0`. Use `6.0` for providers built on Plugin Framework protocol 6.

### GitHub Actions

Exchange the OIDC token for a short-lived Ant token (see [GitHub Actions](../ci/github-actions.md)), then:

```yaml
- run: |
    host=ant_example_com
    echo "TF_TOKEN_${host}=${ANT_TOKEN}" >> "$GITHUB_ENV"
    printf 'machine ant.example.com login ci password %s\n' "$ANT_TOKEN" > ~/.netrc
    chmod 600 ~/.netrc

# then terraform init with a CLI config that contains the host block
```

## Troubleshooting

| Symptom                                                                                                         | Cause                                                                                                                                                             |
| --------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Discovery error (`failed to request discovery document`, or `host does not provide a module/provider registry`) | Terraform asked the host root for `/.well-known/terraform.json`, which Ant does not serve. Add a `host` block or the reverse-proxy mapping.                       |
| `401` on `versions`                                                                                             | No `credentials` block or `TF_TOKEN_*` for the host label in the source address.                                                                                  |
| Module download fails with 401                                                                                  | `~/.netrc` has no entry for the Ant host. The `X-Terraform-Get` archive URL is not signed.                                                                        |
| `401` on `SHA256SUMS` after more than 15 minutes                                                                | The signed URL expired. Run `terraform init` again.                                                                                                               |
| `403` on `PUT`                                                                                                  | The principal lacks Admin on the feed.                                                                                                                            |
| `400` on provider `PUT`                                                                                         | The signature does not verify with `signing_key`, `SHA256SUMS` does not match the zips, a zip has the wrong name or content, or there are more than 12 platforms. |
| `502` on a provider file                                                                                        | A stored file failed re-verification (digest, `SHA256SUMS` line or signature). No bytes are served.                                                               |
| `503` on a proxied provider                                                                                     | The upstream failed, or a redirect to `releases.hashicorp.com` (providers) or `codeload.github.com` (modules) was refused because it is not a redirect origin.    |

Adapter internals: [internal/terraform/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/terraform/README.md).
