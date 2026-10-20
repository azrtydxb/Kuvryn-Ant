# Yum / RPM adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/rpm/` for feeds of format `rpm`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/rpm.md`. The signing secrets are wired with `SetSecrets` when a secret keyring is configured.

```go
repo, err := rpmrepo.New(db, service)
// Check err. Optional: enable repomd.xml.asc signing with per-feed keys.
err = repo.SetSecrets(secrets.Store{DB: db, Keys: keyring})
err = repo.SetPublicURL("https://packages.example")
// Mount at /feeds/{slug}/rpm/.
```

The feed must have format `rpm` and its instance must match the service lease.
Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter. (dnf: `username=`/`password=` in the `.repo`.)

## Routes (under `/feeds/{slug}/rpm/`)

- `PUT Packages/{name}-{version}-{release}.{arch}.rpm` (Admin): ingest one RPM.
  Optional `X-Checksum-Sha256`. Identical retry returns 201; same NEVRA with
  different bytes returns 409.
- `GET/HEAD Packages/{file}.rpm`: byte-exact package with ETag.
- `DELETE Packages/{file}.rpm` (Admin): remove a package (see Deletion).
- `GET/HEAD repodata/repomd.xml`, `repomd.xml.asc`: newest generation.
- `GET/HEAD repodata/{sha256}-{primary|filelists|other}.xml.gz`: content-addressed
  index files, served from whichever generation contains that exact digest.
- `GET/HEAD repodata/repomd.xml.key`: armored public key (404 when unsigned).
- `GET/HEAD ant.repo`: ready-made dnf repo file (needs `SetPublicURL`).

## Ingestion

The body is streamed to a bounded temporary file (2 GiB) and parsed by a pure-Go
RPM reader: lead, signature header and header are bounds-checked (index count,
data size, entry ranges, NUL termination) and nothing is extracted or executed.
Every integrity value the signature header carries is verified: header SHA-256,
header+payload MD5 and the size tags. Name, version, release and arch from the
header must equal the file name. Tampered, truncated or mislabelled input returns
400 and leaves the repository untouched. Existing package signatures stay inside
the untouched bytes (the adapter does not re-sign or verify them; only their
presence is recorded). Name/epoch/version/release/arch, requires/provides/
conflicts/obsoletes (rpmlib() dependencies skipped as createrepo_c does), files
and changelog come from the header.

## Snapshots and atomicity

Core model (namespace `rpm`): one component per package name with a version
`{version}-{release}.{arch}` holding assets `package.rpm` and `metadata.json`
(published atomically through `PublishBundle`; `metadata.json` carries the exact
XML fragments of that package). One component `repodata/snapshot` holds
immutable generations `g{seq:012}-{setHash}`, each a single atomic bundle:
`repomd.xml`, optional `repomd.xml.asc`, and the three checksummed `.xml.gz`
indexes (sha256 checksum and open-checksum, deterministic gzip). Readers always
get `repomd.xml` and its signature from the newest generation, and a client that
fetched an older `repomd.xml` can still fetch that generation's index files, so a
signed index is never mixed with another generation's bytes. A per-feed lock
serializes generation builds; an unchanged package set creates no new generation.
If the index refresh fails after the package committed, the request returns 503
and an identical retry completes it.

Snapshot-owner policy: the first source (priority order) holding any RPM
component owns the whole repository; assets from any other source fail closed.

## Signing

With `SetSecrets`, the first Admin publish creates a per-feed OpenPGP key (RSA,
3072 bits) stored encrypted via `internal/secrets` with purpose
`rpm.repomd-signing-key:{feedID}`; `repomd.xml.asc` is a detached armored
signature over the exact `repomd.xml`. Local keys are never shared with upstream
trust. Use `repo_gpgcheck=1` and `gpgkey=.../repodata/repomd.xml.key`.

## Deletion

Admin only, with an `artifact.delete` audit and outbox event. A generation without
the package is published first, then the version and its assets are removed
(blob bytes are left for retention/GC). Old generations keep listing the package,
so a client holding an old `repomd.xml` can get a 404 for it; there is no
generation pruning yet.

## Known limitations

- The unit and integration tests use synthetic RPMs built in the tests; the real `dnf5`
  client (publish, install, delete, pull-through of a Fedora mirror) is exercised by
  `conformance/run.sh local rpm`.
- No updateinfo/comps/modules ingestion, deltas, or SRPM-specific handling beyond
  `src` arch. Local snapshots are never merged with an upstream repository (see Remote
  repodata). No HTTP Range support. At most 10,000 packages per feed.
- Package (rpm) signatures are retained, not verified against a trust root.
- Signing uses `golang.org/x/crypto/openpgp` (frozen, RSA only).

## Remote sources (pull-through)

The origin is the repository root (the directory that contains `repodata/`). `repomd.xml`, `repomd.xml.asc` and `repomd.xml.key` are served byte for byte from upstream and never rewritten, merged or re-signed, so the client checks the upstream signature itself and package signatures stay inside the untouched RPMs. Any other `repodata/` file is served only if `repomd.xml` lists it with a sha256 checksum, and is checked against it before the first byte is served. A package at any depth is served only if the upstream `primary.xml` lists it with a sha256 checksum, checked before serving and then cached. A repository with local RPMs keeps serving its local snapshot, and a package name owned by a local source is never fetched from upstream. Ant does not verify the upstream `repomd.xml` signature itself.

### Primary indexes and slow upstreams

Primary indexes compressed with gzip, xz, bzip2 or zstd (Fedora) are read to verify package
checksums. A repodata file that must first be fetched from a slow upstream can take longer
than the 30 second per-write deadline; the deadline is lifted for the duration of the fetch
(otherwise an HTTP/2 listener resets the stream with `INTERNAL_ERROR` before the first byte).
