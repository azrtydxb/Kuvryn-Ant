# APT / Debian local adapter

Development subset of F03, not full Phase 1 conformance. Only this package is
implemented; the parent must mount the handler, call `SetKeyring` and
`SetPublicURL`. No migrations or management API changes are required.

```go
repo, err := aptrepo.New(db, service)
err = repo.SetPublicURL("https://packages.example")
repo.SetKeyring(keys) // *secrets.Keyring; required for publish/delete/key export
// Mount at /feeds/{slug}/apt/. The feed must have format `apt`.
```

Auth: scoped Bearer token or HTTP Basic password holding one (`auth.conf`).
Anonymous access is not implemented. Reads are private/no-store.

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
executed. 128 MiB deb cap, 1 MiB control, four concurrent uploads, 50,000
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

- `control.tar.xz`/`.zst` are rejected (no decoder in this build); build with
  `dpkg-deb -Zgzip`.
- Deletion unlists only: bytes stay in the core (no dereference API yet), so a
  pool URL remains downloadable and old by-hash generations stay reachable.
- Valid-Until is not refreshed automatically; the next publish re-signs.
- Source packages (.dsc), Translations/Contents and Range requests on .deb are
  not implemented. Remote/replica sources and a run with the pinned apt 3.0.3
  client are release gates.
