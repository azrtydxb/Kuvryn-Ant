# Ansible Galaxy adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/ansible/` for feeds of format `ansible`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/ansible.md`.

```go
repo, err := ansiblerepo.New(db, service)
// Check err, then configure the externally reachable origin before serving:
err = repo.SetPublicURL("https://galaxy.example")
// Mount at /feeds/{slug}/ansible/.
```

`SetPublicURL` accepts HTTPS or literal loopback HTTP, with no credentials, path,
query or fragment. Host, forwarding headers and request TLS never select URLs.
The feed must have format `ansible`; its instance must match the service lease.
The Galaxy `Token` scheme is treated like `Bearer`. Credentials are verified by the shared client-authentication layer (`internal/identity.ClientAuth`: service tokens, personal access tokens, directory passwords and GitHub OIDC, as the feed's client-auth policy allows) before the request reaches the adapter, which then authorizes through the artifact core. Browser principals are discarded. Anonymous reads of a feed with `anonymous_read` are decided by that layer, not by the adapter. Namespace is the core authorization unit for reads and writes.

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

Concurrent publishes are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32); 64 MiB archive cap, 256 MiB expanded budget, 10,000 tar
members, 2 MiB metadata and 32-member directory depth. Imports expire after one
hour. Streams use 64 KiB buffers and 30-second progress deadlines. Reads are
private/no-store.

## Verification

Run from repository root:

```sh
sh internal/ansiblerepo/test-postgres.sh
```

This follows `scripts/test-postgres.sh`: temporary Unix-socket-only PostgreSQL,
isolated schema, automatic cleanup, and `go test -race -count=1 -v` scoped to this
package. Tests cover publish/import, collection and version indexes, dependency
resolution payloads, byte-identical downloads, tamper rejection, metadata/filename
mismatch and isolation failures.

The real `ansible-galaxy` client (publish, install, pull-through) is exercised by `conformance/run.sh local ansible`. Not implemented: the Galaxy NG testing/approval state machine and collection signature bundles. Import tasks are kept in memory per replica for one hour and are bound to the token and principal that published.

Protocol reference: https://docs.ansible.com/ansible/latest/dev_guide/developing_collections_distributing.html

## Remote sources (pull-through)

The remote source origin is the API root that serves `v3/` (for example `https://galaxy.ansible.com/api/`). Collection detail, the fully paginated versions list and per-version detail are read through the metadata cache and re-rendered with every `href` and `download_url` pointing at this feed; upstream pagination links are consumed, never passed on. A collection archive is fetched on first download from the upstream `download_url`, verified against `artifact.sha256` before the first byte is served and cached with a metadata asset, so cached versions stay listable during an outage. A collection owned by a local source is never answered from upstream.

## Publishing and remote redirects

`ansible-galaxy collection publish` sends the archive base64 encoded
(`Content-Transfer-Encoding: base64` on the `file` part); the part is decoded before the
`sha256` field is checked. galaxy.ansible.com serves `download_url` through a redirect to
its S3 bucket, so a remote source for it needs `https://galaxy.ansible.com` and
`https://ansible-galaxy-ng.s3.dualstack.us-east-1.amazonaws.com` in its redirect origins.
