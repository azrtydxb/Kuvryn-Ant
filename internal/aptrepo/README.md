# APT / Debian adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/apt/` for feeds of format `apt`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/apt.md`. The signing keyring is wired with `SetKeyring` when a secret keyring is configured.

```go
repo, err := aptrepo.New(db, service)
err = repo.SetPublicURL("https://packages.example")
repo.SetKeyring(keys) // *secrets.Keyring; required for publish/delete/key export
// Mount at /feeds/{slug}/apt/. The feed must have format `apt`.
```

Auth: Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter. Reads are private/no-store.

## Routes (under `/feeds/{slug}/apt/`)

- `PUT pool/{component}/{prefix}/{source}/{pkg}_{ver}_{arch}.deb?distribution=D`:
  admin ingestion. Path must be the canonical pool location derived from control.
- `DELETE` the same URL: unlist from D (needs the `delete` action).
- `GET/HEAD` the pool path: byte-identical download with ETag.
- `GET/HEAD dists/D/{Release,InRelease,Release.gpg}`.
- `GET/HEAD dists/D/{comp}/binary-{arch}/Packages[.gz]`, and
  `.../by-hash/SHA256/{hex}`.
- `GET signing-key.asc|signing-key.gpg`: public key for `signed-by`.

## Model

Debs are core assets (`pool/{comp}/{pkg}`, assets `deb` and `control`). Each
index file is stored by content hash (`by-hash/{dist}`) and never removed, so a
client holding an older Release can finish its downloads. A Release generation
(`dists/{dist}`, assets Release/InRelease/Release.gpg, zero-padded version) is
published with `PublishBundle`, which is the single atomic switch: an upload is
invisible to apt until its generation commits. A failed listing can be retried
with the same PUT. State is rebuilt from the current signed Release and its
hash-verified indexes; per-feed in-process locking plus the core lease serialize
writers. `Architecture: all` is merged into every concrete architecture index.

Signing key: RSA-3072 OpenPGP key generated on first publish, sealed with
`internal/secrets` (purpose `apt-signing:{feedID}`), never stored in clear.
Release carries `Date`, `Valid-Until` (30 days, `SetValidity`) and
`Acquire-By-Hash: yes`; SHA256 only.

## Validation and bounds

ar container, `debian-binary` 2.x, one `control.tar` or `control.tar.gz`, one
`data.tar*`; control paragraph strictly parsed (Package/Version/Architecture/
Maintainer/Description, name/version/arch patterns). Maintainer scripts are never
executed. 128 MiB deb cap, 1 MiB control, concurrent uploads limited by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32), 50,000
stanzas per distribution, 64 MiB per index.

## Verification

```sh
sh internal/aptrepo/test-postgres.sh
```

Covers publish/consume with signature verification, by-hash, concurrent
publication with concurrent readers, unlist, authorization, tampered
InRelease/Release/index blobs, wrong key, expired Valid-Until, malformed debs,
and fail-closed signing.

## Known limitations

- `control.tar` is accepted plain or compressed with gzip, xz or zstd (the `dpkg-deb`
  default since dpkg 1.21.18); `data.tar.*` of any compression is stored untouched.
- Deletion unlists only: bytes stay in the core (no dereference API yet), so a
  pool URL remains downloadable and old by-hash generations stay reachable.
- Valid-Until is not refreshed automatically; the next publish re-signs.
- Source packages (.dsc), Translations/Contents and Range requests on .deb are
  not implemented for local content.
- The real apt 3.0.3 client (publish, install, delete, pull-through) is exercised by
  `conformance/run.sh local apt`.

## Remote sources (pull-through)

Release, InRelease and Release.gpg are served verbatim from upstream through the metadata
cache and are never rewritten, merged or re-signed: apt verifies the upstream signature
itself. Every other file under `dists/` is served only if the upstream Release lists it,
fetched by hash when the upstream offers Acquire-By-Hash, verified against the Release
SHA256 before the first byte is served and cached as an immutable asset. A .deb is
served only if a Packages index of a distribution this feed already served lists its pool
path, and is checked against the SHA256 and size of that index.
