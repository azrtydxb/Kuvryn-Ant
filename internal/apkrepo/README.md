# APK (Alpine) adapter

Local Alpine APK v2 repositories (contract F01). Mount: `/feeds/{slug}/apk/`.

| Method | Path | Notes |
| --- | --- | --- |
| PUT | `{branch}/{repo}/{arch}/{name}-{ver}-r{N}.apk` | Feed Admin. Parses `.PKGINFO` from the control segment; name/version must match the filename, arch must equal the path arch or `noarch`. Package + index entry publish as one atomic bundle. Package bytes and signature segment are preserved. |
| GET/HEAD | same | Download. A noarch package (`A:noarch`) is served from every arch path of its branch/repo and from `noarch/`, which is where apk asks for it. |
| DELETE | same | Feed Admin, local sources only; removes version, assets and tags in one transaction with audit and outbox events. Blobs are left for GC. |
| GET | `{branch}/{repo}/{arch}/APKINDEX.tar.gz` | Generated on request from committed entries, signed `.SIGN.RSA.ant-<feed8>.rsa.pub` (RSA-2048, SHA-1 over the compressed index segment). |
| GET | `keys/ant-<feed8>.rsa.pub` | PEM public key for `/etc/apk/keys`. |

Auth: Bearer or Basic (password = token), same as the other adapters. Index, key and package reads require Read.

Signing key: generated lazily per feed, stored as PKCS#8 via `internal/secrets` (purpose `apk-feed-signing-key:<feed id>`, no migration). Wire with `SetKeyring(*secrets.Keyring)`; without it, the index and key endpoints return 503.

Client setup:

    wget -O /etc/apk/keys/ant-XXXXXXXX.rsa.pub https://host/feeds/alpine/apk/keys/ant-XXXXXXXX.rsa.pub
    echo https://host/feeds/alpine/apk/v3.24/main >> /etc/apk/repositories

## Limitations

- No real `apk` client conformance run yet (apk-tools 3.24.2 fixture pending); tests verify the signature and format in Go.
- Uploaded package signatures are preserved but not verified (no trusted-key import yet).
- Local channels: the index is regenerated per request (max 4096 packages per channel).
- Remote sources (pull-through) are described below.
- v3 wire format is out of scope.

Run: `sh internal/apkrepo/test-postgres.sh`.

## Remote sources (pull-through)

The remote source origin is the repository root that holds `<branch>/<repository>/<arch>/`, for example
`https://dl-cdn.alpinelinux.org/alpine`. The trust model is the one of APT and RPM:

- `APKINDEX.tar.gz` is served byte for byte from upstream (through the metadata cache). Ant never rewrites, merges or
  re-signs it, so `apk` verifies the upstream signature with the upstream key in `/etc/apk/keys`. Ant's own feed key signs
  only locally published channels; `keys/` serves that key only.
- A package is served only if the upstream index of its channel lists it. Before the first byte is served its control segment
  is checked against the index checksum (`C:Q1` + base64 SHA-1 of the control segment, the identity `apk` itself uses), its size
  against `S:` and its name/version/arch against the file name; it is then cached under the remote source. Signature segments
  inside the package are preserved. A mismatch is a 503 and nothing is cached.
- A channel has one owner: a channel with locally published packages is never extended from upstream.

Limits: indexes (and therefore "which packages exist") come from the in-process metadata cache, so beyond its stale-if-error window
an upstream outage makes `apk update` fail even though cached packages remain downloadable; compressed indexes above the metadata
limit cannot be proxied (Alpine `main` and `community` are well below the default 24 MiB used by the server). Ant does not hold the
upstream signing keys and does not verify the index signature itself. Cached remote packages are not part of the locally
generated index. Remote packages are limited to 1 GiB.
