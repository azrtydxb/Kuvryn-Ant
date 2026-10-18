# Import, export and migration

Covers product spec section 9 (migration and staged adoption). Backups
(`ant backup`) copy a whole deployment; this tooling moves selected feeds
between instances or brings repositories in from another artifact manager.

## Export and import bundles

```sh
ant export [--feed SLUG]... [--namespace NS]... [--no-content] <new-or-empty-dir>
ant export verify [--expect-manifest-sha256 HEX] <dir>
ant import [--apply] [--expect-manifest-sha256 HEX] <dir>
```

A bundle contains `document.json` (feeds, sources, ordered feed sources,
anonymous-read flag, publish policies, retention policies, remote source
configuration, and the inventory of local content with checksums, paths,
namespaces and format metadata), `blobs/<sha256>` (content bytes) and
`manifest.json` (written last; digests of the document and every blob).
Record the manifest digest printed by `ant export` and pass it back with
`--expect-manifest-sha256` to detect replacement of a whole bundle.

Never exported: credential values, root keys, token secrets. A remote source
carries only a `credential_ref`; after import the report lists it under
"unmapped" and the secret is provisioned with
`PUT /api/v1/sources/{id}/remote-credential`. Selectors that name a principal ID
are dropped (IDs are not portable) and reported.

`ant import` is a dry run unless `--apply` is given. The dry run executes the
same code as the apply inside a rolled-back transaction, so its report (created,
updated, unchanged, skipped, conflict per object; unmapped configuration;
integrity failures) is exactly what apply does. Apply is idempotent: running it
again reports everything unchanged. Existing content is never overwritten; a
different digest at an existing path is a conflict. A blob that fails digest or
size verification is reported and its asset skipped. The command exits 2 when
integrity failures are found.

Remote caches and replica sources export definitions only; their content is
re-fetched or re-replicated. Replica sources are not imported (they need a
replication peer).

## Migrating from Nexus 3

```sh
export ANT_NEXUS_PASSWORD=...   # or ANT_NEXUS_TOKEN
ant migrate nexus --url https://nexus.example.com --user admin [--repo NAME]... [--no-count]
ant migrate nexus --url ... --user admin --copy-content --apply
ant compare nexus --url ... --user admin --coordinates coords.txt
```

Mapping: hosted becomes a local source, proxy a remote source (origin, metadata
TTL; an authenticated proxy is reported so its credential can be re-entered),
group a feed with its members as ordered sources (nested groups flattened,
writable member becomes the publish default), and every hosted or proxy
repository also gets a same-named single-source feed so client URLs can be
kept. Cleanup policies map to retention (last updated to max age, last
downloaded to max idle, retain to keep last); regex and prerelease-only
criteria and additional policies are reported as unmapped.

`--copy-content` downloads the assets of hosted repositories through their
download URLs and verifies each against Nexus's SHA-256 before it is stored
(raw and Maven layouts only; other formats are mapped as definitions and
reported). Maven metadata files are not copied because Ant generates them.
Credentials come from the environment at run time and are never stored; they
are only ever sent to the origin of `--url`.

`ant compare` reads lines of `<repository> <path> [<feed>]` and reports, for each
coordinate, the SHA-256 a client would receive from Nexus and from the Ant feed
(first source in feed order holding the path): identical, differs,
missing_in_ant, missing_in_source, missing_in_both. Use it during staged
adoption before switching clients.

The `Manager` interface in `internal/transfer/migrate.go` is the extension point
for other managers (for example Artifactory); only Nexus is implemented.

## Management API and console

`POST /api/v1/transfer/export`, `POST /api/v1/transfer/import` (dry run by
default), `POST /api/v1/migrations/nexus`, `POST /api/v1/migrations/compare`
and `GET /api/v1/transfer/runs` require instance admin. The API handles
definitions; content bytes go through the CLI. Run history keeps counts only.
The console screen "Import & migrate" drives the same endpoints.
