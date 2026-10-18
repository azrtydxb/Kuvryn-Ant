# Ansible Galaxy local adapter

Development subset of F02, not full Phase 1 conformance. Only this package is
implemented here; parent integration must mount the handler and configure the
trusted origin. No migrations, management API, main wiring or dependency changes
are required by this package.

```go
repo, err := ansiblerepo.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://galaxy.example")
// Mount at /feeds/{slug}/ansible/ using the parent router.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `ansible`; its instance must match the service lease.
Every request uses a scoped machine Bearer token or Galaxy `Token` scheme. Browser
principals are discarded. Anonymous access is not implemented. Namespace is the
core authorization unit for reads and writes.

## Routes

- `POST v3/artifacts/collections`: multipart publish of a built collection. The
  `sha256` field must exactly match the `file` archive; filenames follow
  `namespace-name-version.tar.gz`.
- `GET/HEAD v3/imports/{id}/`: import task state until completed.
- `GET/HEAD v3/collections/{ns}/{name}/`: collection detail.
- `GET/HEAD v3/collections/{ns}/{name}/versions/`: version list.
- `GET/HEAD v3/collections/{ns}/{name}/versions/{version}/`: version metadata with
  download URL, artifact SHA-256 and dependencies.
- `GET/HEAD downloads/{ns}/{name}/{version}/collection.tar.gz`: immutable archive.
- `GET/HEAD v3/` and `/`: Galaxy version discovery.

## Ingestion and atomicity

`MANIFEST.json`, `FILES.json` and the complete tar inventory are validated without
executing Python, Jinja, plugins or hooks. The archived file checksums must match
`FILES.json`, and `FILES.json`'s digest must match `MANIFEST.json`. Every member is
suite-consumed and cross-checked; no member may be missing, extra, duplicated or
trailing. The identifier in the archive must match the namespace/name/version of
the filename and the upload digest. The `archive` and generated `metadata` assets
publish atomically through `PublishBundle` with the pinned source. Identical
retries return a completed import without new core events.

## Bounds

Four concurrent publishes, 64 MiB archive cap, 256 MiB expanded budget, 10,000 tar
members, 2 MiB metadata and 32-member directory depth. Imports expire after one
hour. Streams use 64 KiB buffers and 30-second progress deadlines. Reads are
private/no-store.

## Verification and remaining gates

Run from repository root:

```sh
sh internal/ansiblerepo/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests cover publish/import, collection and version indexes, dependency
resolution payloads, byte-identical downloads, tamper rejection, metadata/filename
mismatch and isolation failures.

The package has not been exercised against the real ansible-galaxy client; native
client verification with the pinned ansible-core baseline, remote/replica
resolution, testing/import state machine parity, signature bundles, retention and
deployment evidence remain release gates. All 26 formats remain Phase 1 scope.
Protocol reference: https://docs.ansible.com/ansible/latest/dev_guide/developing_collections_distributing.html