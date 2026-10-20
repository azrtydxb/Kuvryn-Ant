# APT (Debian / Ubuntu)

The APT adapter serves signed Debian binary repositories to `apt` and `apt-get`. A feed can hold packages you upload yourself, each distribution signed with a key that belongs to that feed, and it can proxy and cache public archives such as `deb.debian.org` or `archive.ubuntu.com`, passing their signed metadata through unchanged.

Endpoint: `https://ant.example.com/feeds/<slug>/apt/` (feed format `apt`). Point the repository URL in `sources.list` at this path.

## Support matrix

| Publish                                              | Consume                               | Remote (pull-through)                                              | Replica                       | Notes                                              |
| ---------------------------------------------------- | ------------------------------------- | ------------------------------------------------------------------ | ----------------------------- | -------------------------------------------------- |
| Yes, `PUT` of a `.deb` to its pool path (feed admin) | Yes, `apt`/`apt-get` with `signed-by` | Yes, the upstream `Release`/`InRelease` is passed through verbatim | Yes, through core replication | Binary packages only. No source packages (`.dsc`). |

Served routes (all under `/feeds/<slug>/apt/`):

| Method        | Path                                                             | Purpose                                            |
| ------------- | ---------------------------------------------------------------- | -------------------------------------------------- |
| `GET`, `HEAD` | `dists/<dist>/Release`, `InRelease`, `Release.gpg`               | Signed release files                               |
| `GET`, `HEAD` | `dists/<dist>/<component>/binary-<arch>/Packages`, `Packages.gz` | Package indexes                                    |
| `GET`, `HEAD` | `dists/<dist>/<component>/binary-<arch>/by-hash/SHA256/<hex>`    | Indexes addressed by hash (`Acquire-By-Hash: yes`) |
| `GET`, `HEAD` | `pool/<component>/<prefix>/<source>/<pkg>_<ver>_<arch>.deb`      | Download a package                                 |
| `PUT`         | the same pool path, plus `?distribution=<dist>`                  | Upload a package and list it in `<dist>`           |
| `DELETE`      | the same pool path, plus `?distribution=<dist>`                  | Remove the package from `<dist>`                   |
| `GET`         | `signing-key.asc`, `signing-key.gpg`                             | The feed's public signing key (armored or binary)  |

## Limits

- One `.deb` can be at most 128 MiB. The control member can be at most 1 MiB. Each distribution holds at most 50,000 stanzas, and each index can be at most 64 MiB. Concurrent uploads per process are bounded by `ANT_UPLOAD_SLOTS` (default: twice the CPU count, between 4 and 32). An upload beyond that answers 503.
- `control.tar` can be plain or compressed with gzip, xz or zstd. `data.tar.*` is stored as uploaded, whatever its compression. Maintainer scripts are never run.
- Only SHA256 appears in `Release`. `Architecture: all` packages are merged into the index of every concrete architecture.
- A local `Release` carries `Valid-Until` 30 days after the last publish. Nothing refreshes it automatically: the next publish to that distribution re-signs it. See [Troubleshooting](#troubleshooting).
- Deleting a package only unlists it. The bytes stay in storage, so the pool URL and older by-hash generations remain downloadable until retention removes them.
- The server does not produce source packages, `Translation-*` files or `Contents-*` files, and it does not answer Range requests on `.deb` downloads.
- Responses carry `Cache-Control: private, no-store`.

::: warning Gap
The signing key and pool layout are covered by integration tests and by the real `apt` 3.0.3 conformance run. The recorded 2026-10-19 run failed on a `.deb` built with the `dpkg-deb` zstd default. The code now accepts zstd `control.tar`, but no later real-client run has confirmed it.
:::

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `apt`. Create a local source on **Sources**, attach it to the feed, and make it the default target of the feed's publish policy. For a proxy, create a remote source instead, then set its origin on the source's **Upstream** tab.

With the management API:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# local source for uploaded packages
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: apt-local-1' \
  -d '{"name":"apt-local","kind":"local","format":"apt"}' | jq -r .id)

# feed "debian-internal" with that source as publish target
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: apt-feed-1' -d @- <<JSON
{"slug":"debian-internal","format":"apt","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

To learn how sources, read order and publish targets fit together, see [Feeds and sources](../concepts/feeds-and-sources.md) and [Publish policies](../concepts/publish-policies.md).

## Remote upstreams

The origin of a remote APT source is the repository root, the directory that contains `dists/` and `pool/`. One root can serve many distributions (`bookworm`, `bookworm-updates`, `trixie`, ...).

Trust model:

- The adapter serves upstream `Release`, `InRelease` and `Release.gpg` byte for byte. It never rewrites, merges or re-signs them, so `apt` checks the signature against the upstream archive key you name in `signed-by`.
- Any other `dists/` file is served only if the upstream `Release` lists it. The file is fetched by hash when upstream advertises `Acquire-By-Hash`, checked against the SHA256 in `Release`, and cached under its hash.
- A `.deb` is served only if it is listed in a `Packages` index of a distribution that this feed has already served. Its SHA256 is checked before the client receives any bytes, and then the package is cached.
- Ant does not check the upstream `Release` signature itself, because no upstream key is configured on the server. The integrity of upstream bytes rests on TLS to the origin plus the client's own signature check.
- A distribution that has locally published packages is never answered from upstream.

Worked examples (each one is a separate remote source; one feed can list several of them in read order):

| Upstream                        | `origin`                                | `redirect_origins` | Client `signed-by`                               |
| ------------------------------- | --------------------------------------- | ------------------ | ------------------------------------------------ |
| Debian                          | `https://deb.debian.org/debian`         | none               | `/usr/share/keyrings/debian-archive-keyring.gpg` |
| Ubuntu (amd64)                  | `https://archive.ubuntu.com/ubuntu`     | none               | `/usr/share/keyrings/ubuntu-archive-keyring.gpg` |
| Ubuntu security                 | `https://security.ubuntu.com/ubuntu`    | none               | `/usr/share/keyrings/ubuntu-archive-keyring.gpg` |
| Ubuntu ports (arm64 and others) | `https://ports.ubuntu.com/ubuntu-ports` | none               | `/usr/share/keyrings/ubuntu-archive-keyring.gpg` |

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: apt-debian-1' \
  -d '{"name":"debian","kind":"remote","format":"apt"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: apt-debian-cfg-1' -d '{
  "origin":"https://deb.debian.org/debian","redirect_origins":[],"allow_private":false,
  "timeout_seconds":120,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":86400,
           "max_entries":10000,"max_bytes":268435456}}'

curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: apt-debian-feed-1' -d @- <<JSON
{"slug":"debian","format":"apt","source_ids":["$REM"]}
JSON
```

Private upstreams that need authentication take a credential (`PUT /api/v1/sources/{id}/remote-credential` with `{"authorization":"Basic ..."}` or `{"authorization":"Bearer ..."}`). The credential is stored encrypted, never returned, and only ever sent to the source origin.

::: warning Gap

- Before any package can be pulled, `apt update` must run through the feed at least once. The feed only learns about pool paths from the `Packages` indexes it has served.
- The package route accepts exactly five segments, `pool/<component>/<prefix>/<source>/<file>.deb`. The `debian-security` archive lists pool paths with an extra level (`pool/updates/main/...`). From reading the code, such paths do not match the route. Proxying `https://deb.debian.org/debian-security` has not been verified.
- A cached `.deb` stays downloadable while the upstream is down. Indexes come from the in-process metadata cache, so once the stale-if-error window ends, `apt update` fails during an outage.

:::

## Client configuration

### Credentials

APT authenticates with HTTP Basic. Use any username and an Ant token as the password: a service token (`ant_...`) or a personal access token (`antp_...`). Put the credentials in `/etc/apt/auth.conf.d/`, never in `sources.list`:

```ini
# /etc/apt/auth.conf.d/ant.conf  (mode 0600, owned by root)
machine ant.example.com/feeds/debian-internal/apt/ login token password <TOKEN>
```

Without a protocol prefix, an `auth.conf` entry applies to `https` only. Reading packages needs the `read` permission on the feed. If the feed has anonymous reads enabled and the client auth policy allows `anonymous`, you can leave the file out. See [Package client auth](../identity/client-auth.md).

### Local feed: install the signing key

On the first publish, each feed gets its own RSA-3072 OpenPGP key. The key is generated on the server and stored encrypted (sealed with the instance key ring). It is never stored or returned in clear: only the public part can be downloaded. Until the first publish, `signing-key.asc` and `signing-key.gpg` answer 404. Downloading the key needs read access:

```sh
curl -fsS -u token:$ANT_TOKEN \
  https://ant.example.com/feeds/debian-internal/apt/signing-key.gpg \
  -o /usr/share/keyrings/ant-debian-internal.gpg
```

::: code-group

```ini [deb822 (.sources)]
# /etc/apt/sources.list.d/ant-internal.sources
Types: deb
URIs: https://ant.example.com/feeds/debian-internal/apt
Suites: stable
Components: main
Signed-By: /usr/share/keyrings/ant-debian-internal.gpg
```

```sh [one-line (.list)]
# /etc/apt/sources.list.d/ant-internal.list
deb [signed-by=/usr/share/keyrings/ant-debian-internal.gpg] https://ant.example.com/feeds/debian-internal/apt stable main
```

:::

`Suites` is the distribution name you used on upload (`?distribution=stable`). `Components` is the component segment of the pool path (`main` above).

### Remote feed: keep the upstream key

The upstream `Release` is not re-signed, so keep the distribution's own keyring:

::: code-group

```ini [Debian (.sources)]
# /etc/apt/sources.list.d/debian.sources
Types: deb
URIs: https://ant.example.com/feeds/debian/apt
Suites: trixie trixie-updates
Components: main
Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg
```

```ini [Ubuntu (.sources)]
# /etc/apt/sources.list.d/ubuntu.sources
Types: deb
URIs: https://ant.example.com/feeds/ubuntu/apt
Suites: noble noble-updates
Components: main restricted universe
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
```

:::

### Publish

Uploading needs the `admin` action on the feed: the adapter checks feed admin, not just `publish`. The path must be the canonical Debian pool location of the package, built from its control file:

- `<component>` is the target component, for example `main`.
- `<prefix>` is the first letter of the source package name, or the first four letters (`libf`) when the name starts with `lib`.
- `<source>` is the `Source:` field, or `Package:` when there is no `Source:` field.
- The file name is `<Package>_<Version without epoch>_<Architecture>.deb`.

```sh
curl -fsS -u token:$ANT_TOKEN -X PUT \
  -H "X-Checksum-Sha256: $(sha256sum hello-ant_1.0_amd64.deb | cut -d' ' -f1)" \
  --data-binary @hello-ant_1.0_amd64.deb \
  "https://ant.example.com/feeds/debian-internal/apt/pool/main/h/hello-ant/hello-ant_1.0_amd64.deb?distribution=stable"
```

`distribution` must be the only query parameter. An upload stays invisible to `apt` until the new signed `Release` generation commits. An identical retry answers 201. The same pool path with different bytes answers 409. To remove a package from a distribution, send `DELETE` to the same URL; this needs the `delete` action.

### GitHub Actions

Exchange the workflow's OIDC token for a short-lived Ant token (`POST /api/v1/auth/github-oidc/exchange`, matched against a trust rule), then write it into `auth.conf.d`:

```yaml
permissions:
  id-token: write
  contents: read
steps:
  - name: Ant token
    run: |
      JWT=$(curl -sS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ant.example.com" | jq -r .value)
      ANT=$(curl -sS -X POST https://ant.example.com/api/v1/auth/github-oidc/exchange \
        -H 'Content-Type: application/json' -d "{\"token\":\"$JWT\"}" | jq -r .access_token)
      echo "::add-mask::$ANT"
      echo "ANT_TOKEN=$ANT" >> "$GITHUB_ENV"
  - run: |
      printf 'machine ant.example.com/feeds/debian-internal/apt/ login token password %s\n' "$ANT_TOKEN" \
        | sudo tee /etc/apt/auth.conf.d/ant.conf >/dev/null
      sudo chmod 600 /etc/apt/auth.conf.d/ant.conf
```

The `azrtydxb/ant-setup-action` action wraps the exchange step. [GitHub Actions](../ci/github-actions.md) covers the action, and [GitHub OIDC trust](../identity/github-oidc.md) covers the trust rules.

## Troubleshooting

| Symptom                                                        | Cause                                                                                                                                                                                       |
| -------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `401 Unauthorized` on `Release`                                | No credentials or wrong credentials. Check that the `auth.conf.d` `machine` path matches the feed URL.                                                                                      |
| `403`                                                          | The token lacks read permission, the feed's client auth policy refuses the credential type, or you uploaded without feed admin.                                                             |
| `Release file ... is expired`                                  | A local distribution was last published more than 30 days ago. Publish to it again to re-sign, or set `Acquire::Check-Valid-Until "false";` for that source as a stopgap.                   |
| `NO_PUBKEY` or `The following signatures couldn't be verified` | The wrong keyring is in `signed-by`. A local feed needs the feed's `signing-key.gpg`; a remote feed needs the upstream archive keyring.                                                     |
| `400` on `PUT`                                                 | The path is not the canonical pool path for the control file, the file is not a valid `.deb`, `distribution` is missing or invalid, or `X-Checksum-Sha256` does not match.                  |
| `409` on `PUT`                                                 | The same pool path is already listed with different bytes.                                                                                                                                  |
| `404` for a remote `.deb`                                      | The feed has not served that distribution's `Packages` yet (run `apt update` through the feed), or upstream does not list it.                                                               |
| `503`                                                          | An upstream error while nothing is cached, or the server has no secret key ring (publishing fails closed).                                                                                  |
| `X-Kuvryn-Cache: stale` with `Warning: 110`                    | The upstream `Release` came from the metadata cache after its TTL expired, because upstream was unreachable. `X-Kuvryn-Cache: offline` means the feed fell back to its locally cached view. |

Adapter internals: [internal/aptrepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/aptrepo/README.md).
