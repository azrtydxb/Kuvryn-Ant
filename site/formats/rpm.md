# RPM (DNF / YUM / Zypper)

The RPM adapter serves RPM-MD (`repodata/repomd.xml`) repositories to `dnf`, `yum` and `zypper`. A feed can hold RPMs you upload, with `repomd.xml` signed by a key that belongs to that feed. It can also proxy and cache an upstream repository such as a Rocky Linux, AlmaLinux, Fedora or EPEL tree, passing its signed metadata through unchanged.

Endpoint: `https://ant.example.com/feeds/<slug>/rpm/` (feed format `rpm`). This URL is the repository root, used as `baseurl`.

## Support matrix

| Publish                                      | Consume            | Remote (pull-through)                                 | Replica                       | Notes                                               |
| -------------------------------------------- | ------------------ | ----------------------------------------------------- | ----------------------------- | --------------------------------------------------- |
| Yes, `PUT Packages/<nevra>.rpm` (feed admin) | Yes, dnf5 verified | Yes, upstream `repomd.xml` is passed through verbatim | Yes, through core replication | One feed is one repository (one architecture tree). |

Served routes (all under `/feeds/<slug>/rpm/`):

| Method        | Path                                                           | Purpose                                                              |
| ------------- | -------------------------------------------------------------- | -------------------------------------------------------------------- |
| `GET`, `HEAD` | `repodata/repomd.xml`, `repodata/repomd.xml.asc`               | The newest generation and its detached signature                     |
| `GET`, `HEAD` | `repodata/repomd.xml.key`                                      | The feed's armored public key (404 while the repository is unsigned) |
| `GET`, `HEAD` | `repodata/<sha256>-primary.xml.gz` (also `filelists`, `other`) | Index files, addressed by content                                    |
| `GET`, `HEAD` | `Packages/<name>-<version>-<release>.<arch>.rpm`               | Download a local package                                             |
| `GET`, `HEAD` | `<any path>/<file>.rpm` (for example `Packages/a/acl-...rpm`)  | Packages of a remote repository, at their upstream layout            |
| `PUT`         | `Packages/<name>-<version>-<release>.<arch>.rpm`               | Upload one RPM                                                       |
| `DELETE`      | `Packages/<name>-<version>-<release>.<arch>.rpm`               | Remove one RPM (feed admin)                                          |
| `GET`, `HEAD` | `ant.repo`                                                     | A ready-made `.repo` file for this feed                              |

## Limits

- One upload can be at most 2 GiB, and a feed can hold at most 10,000 packages. The server never extracts or runs anything from an RPM.
- Every integrity value in the signature header is checked on upload: the header SHA-256, the header-plus-payload MD5 and the size tags. Name, version, release and arch in the header must equal the file name. Package signatures stay inside the stored bytes. They are recorded as present but not checked against a trust root.
- There is no ingestion of `updateinfo`, `comps` or modules, no deltas, and no HTTP Range support.
- Deletion publishes a new generation without the package, then removes the version. Older generations still list it, so a client that holds an older `repomd.xml` can get a 404 for that package. Old generations are never pruned.
- Signing uses RSA keys through `golang.org/x/crypto/openpgp`.
- Responses carry `Cache-Control: private, no-store`.

## Create the feed

In the console, open **Feeds**, choose **New feed** and pick format `rpm`. Attach a local source (for uploads) or a remote source (for a proxy). Then set the publish policy for a local feed, or the origin on the remote source's **Upstream** tab.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: rpm-local-1' \
  -d '{"name":"rpm-local","kind":"local","format":"rpm"}' | jq -r .id)
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: rpm-feed-1' -d @- <<JSON
{"slug":"rpm-internal","format":"rpm","source_ids":["$SRC"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

The first source in read order that holds any RPM owns the whole repository. Assets from any other source attached to the same feed are refused. See [Feeds and sources](../concepts/feeds-and-sources.md).

## Remote upstreams

The origin of a remote RPM source is the repository root, the directory that contains `repodata/`. Because each upstream repository (BaseOS, AppStream, EPEL, ...) has its own `repomd.xml`, use one feed per upstream repository and per architecture. If you attach several remote sources to one feed, the first one that answers `repomd.xml` serves the whole repository.

Trust model:

- The adapter serves `repomd.xml`, `repomd.xml.asc` and `repomd.xml.key` from upstream byte for byte. It never rewrites, merges or re-signs them. The client checks upstream's repository signature (`repo_gpgcheck`) itself, and package signatures (`gpgcheck`) stay inside the untouched RPMs.
- Any other `repodata/` file is served only if `repomd.xml` lists it with a sha256 checksum. It is checked against that checksum before the first byte is served, then cached under its hash.
- A package at any depth (`Packages/a/acl-...rpm`) is served only if the upstream `primary.xml` lists it with a sha256 checksum. The bytes are checked before serving, then cached.
- `primary.xml` compressed with gzip, xz, bzip2 or zstd (Fedora) can be read.
- A repository with local RPMs keeps serving its local snapshot, and a package name owned by a local source is never fetched from upstream.
- Ant does not check the upstream `repomd.xml` signature itself.

Worked examples:

| Upstream                | `origin` (x86_64)                                                                | `redirect_origins` | Package signing key on the client          |
| ----------------------- | -------------------------------------------------------------------------------- | ------------------ | ------------------------------------------ |
| Rocky Linux 9 BaseOS    | `https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os`                         | none               | `/etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9`     |
| Rocky Linux 9 AppStream | `https://dl.rockylinux.org/pub/rocky/9/AppStream/x86_64/os`                      | none               | `/etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9`     |
| AlmaLinux 9 BaseOS      | `https://repo.almalinux.org/almalinux/9/BaseOS/x86_64/os`                        | none               | `/etc/pki/rpm-gpg/RPM-GPG-KEY-AlmaLinux-9` |
| EPEL 9                  | `https://dl.fedoraproject.org/pub/epel/9/Everything/x86_64`                      | none               | the `epel-release` key                     |
| Fedora 41 release       | `https://dl.fedoraproject.org/pub/fedora/linux/releases/41/Everything/x86_64/os` | none               | the `fedora-gpg-keys` key                  |

Pin a fixed mirror host as the origin. Mirror-selecting front doors such as `download.fedoraproject.org` or `mirrors.rockylinux.org` redirect each request to a different mirror host. A redirect is followed only to a host in `redirect_origins`, at most five hops, and the credential never travels with it. Each possible mirror would therefore have to be listed, which the 16-entry limit makes impractical.

```sh
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: rocky9-baseos-1' \
  -d '{"name":"rocky9-baseos","kind":"remote","format":"rpm"}' | jq -r .id)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: rocky9-baseos-cfg-1' -d '{
  "origin":"https://dl.rockylinux.org/pub/rocky/9/BaseOS/x86_64/os","redirect_origins":[],
  "allow_private":false,"timeout_seconds":120,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":86400,
           "max_entries":10000,"max_bytes":268435456}}'
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: rocky9-baseos-feed-1' -d @- <<JSON
{"slug":"rocky9-baseos","format":"rpm","source_ids":["$REM"]}
JSON
```

A private upstream (for example a vendor repository behind Basic auth) takes a credential through `PUT /api/v1/sources/{id}/remote-credential`, such as `{"authorization":"Basic <base64>"}`. The credential is stored encrypted, never returned, and only sent to the origin.

::: warning Gap

- Only Rocky Linux 9 BaseOS has been verified with a real client (dnf5, through the conformance harness). The AlmaLinux, EPEL and Fedora rows are built from the same code path but have not been run.
- Checksums other than sha256 in `repomd.xml` or `primary.xml` are refused. Old repositories that publish only sha1 cannot be proxied.
- The upstream index lives only in the in-process metadata cache. Once the stale-if-error window ends, `dnf makecache` fails during an upstream outage, although cached RPMs stay downloadable.

:::

## Client configuration

### Credentials

Use HTTP Basic with any username and an Ant token (service token or personal access token) as the password. Reading needs the `read` permission on the feed. Uploading and deleting need the `admin` action on the feed. Anonymous reads work only if the feed has anonymous reads enabled and its client auth policy allows `anonymous` (see [Package client auth](../identity/client-auth.md)).

### Signing key (local feeds)

When the server has a secret key ring, the first publish creates a per-feed OpenPGP key (RSA 3072). The key is stored encrypted, and from then on `repomd.xml.asc` is a detached signature over the exact `repomd.xml`. The public key is at `repodata/repomd.xml.key`. It needs read access, so download it once and refer to it locally:

```sh
curl -fsS -u token:$ANT_TOKEN \
  https://ant.example.com/feeds/rpm-internal/rpm/repodata/repomd.xml.key \
  -o /etc/pki/rpm-gpg/RPM-GPG-KEY-ant-rpm-internal
```

`GET ant.repo` returns a ready-made file with `baseurl`, plus `repo_gpgcheck=1` and a `gpgkey` URL when the feed is signed. It does not include credentials, and its `baseurl` comes from `ANT_PUBLIC_URL`.

::: code-group

```ini [dnf / yum]
# /etc/yum.repos.d/ant-rpm-internal.repo
[ant-rpm-internal]
name=Kuvryn Ant rpm-internal
baseurl=https://ant.example.com/feeds/rpm-internal/rpm/
enabled=1
# packages uploaded to Ant keep whatever signature they were built with
gpgcheck=0
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-ant-rpm-internal
username=token
password=<TOKEN>
```

```ini [zypper]
# /etc/zypp/repos.d/ant-rpm-internal.repo
[ant-rpm-internal]
name=Kuvryn Ant rpm-internal
baseurl=https://ant.example.com/feeds/rpm-internal/rpm/?credentials=ant
type=rpm-md
enabled=1
autorefresh=1
gpgcheck=0
repo_gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-ant-rpm-internal

# /etc/zypp/credentials.d/ant  (mode 0600)
# username=token
# password=<TOKEN>
```

:::

If your RPMs are signed at build time, set `gpgcheck=1` and add the build key to `gpgkey`.

::: warning Gap
Only dnf5 has been run against the adapter. The `yum` and `zypper` files above use those clients' standard syntax but have not been tested against Ant. It is also unverified whether dnf sends the repository's `username`/`password` when it fetches an `https://` `gpgkey` URL. That is why the examples download the key once and refer to it with `file://`.
:::

### Remote feeds

Keep the distribution's own keys. `repomd.xml.asc` comes from upstream, so set `repo_gpgcheck=1` only if the upstream signs `repomd.xml`. Rocky, AlmaLinux, Fedora and EPEL sign packages, which `gpgcheck=1` covers.

```ini
# /etc/yum.repos.d/rocky9-baseos.repo
[rocky9-baseos]
name=Rocky Linux 9 BaseOS via Ant
baseurl=https://ant.example.com/feeds/rocky9-baseos/rpm/
enabled=1
gpgcheck=1
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9
username=token
password=<TOKEN>
```

### Publish

```sh
curl -fsS -u token:$ANT_TOKEN -X PUT \
  -H "X-Checksum-Sha256: $(sha256sum hello-ant-1.0-1.noarch.rpm | cut -d' ' -f1)" \
  --data-binary @hello-ant-1.0-1.noarch.rpm \
  https://ant.example.com/feeds/rpm-internal/rpm/Packages/hello-ant-1.0-1.noarch.rpm
```

A new package answers 201, and an identical retry also answers 201. The same NEVRA with different bytes answers 409. The new `repomd.xml` generation is built under a per-feed lock. If the index refresh fails after the package committed, the request answers 503 and an identical retry completes it.

### GitHub Actions

Get a short-lived token with the OIDC exchange described in [GitHub Actions](../ci/github-actions.md), then use it as the password in the `.repo` file or in the `curl -u token:$ANT_TOKEN` upload. Trust rules are covered in [GitHub OIDC trust](../identity/github-oidc.md).

## Troubleshooting

| Symptom                                                   | Cause                                                                                                                                            |
| --------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------ |
| `401` on `repomd.xml` (dnf: "Cannot download repomd.xml") | Missing or wrong `username`/`password`.                                                                                                          |
| `403`                                                     | The token lacks read permission, the client auth policy refuses that credential type, or you uploaded or deleted without feed admin.             |
| `400` on `PUT`                                            | The file name does not match the header NEVRA, the RPM is truncated or tampered with, or `X-Checksum-Sha256` does not match.                     |
| `409` on `PUT`                                            | The same NEVRA already exists with different bytes.                                                                                              |
| `503` on `ant.repo`                                       | `ANT_PUBLIC_URL` is not set.                                                                                                                     |
| `404` on a remote package                                 | Upstream `primary.xml` does not list it with a sha256 checksum, or a local source owns that package name.                                        |
| `503` on a remote feed                                    | Upstream error with nothing cached, or a checksum mismatch.                                                                                      |
| Signature errors on a remote feed with `repo_gpgcheck=1`  | The upstream does not sign `repomd.xml`. Set `repo_gpgcheck=0` and keep `gpgcheck=1`.                                                            |
| `X-Kuvryn-Cache: stale` and `Warning: 110`                | `repomd.xml` came from the metadata cache after its TTL because upstream was unreachable. `offline` marks a fallback to the locally cached view. |

Adapter internals: [internal/rpmrepo/README.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/rpmrepo/README.md).
