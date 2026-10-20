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
for other managers; Nexus 3 and Artifactory are implemented.

## Migrating from Artifactory

```sh
export ANT_ARTIFACTORY_TOKEN=...      # access token (Bearer), or:
export ANT_ARTIFACTORY_PASSWORD=...   # password or API key, with --user
ant migrate artifactory --url https://example.jfrog.io/artifactory [--repo KEY]... [--ca-ref ID] [--no-count]
ant migrate artifactory --url ... --copy-content --apply
ant compare artifactory --url ... --coordinates coords.txt
```

The URL is the instance URL; `/artifactory` is appended when it has no path.
Authentication is an access token (Bearer) or a user with password or API key
(basic). `--ca-ref` (API: `ca_ref`) names a CA bundle from the operator's
`ANT_UPSTREAM_CA_MAP_FILE`; a request can never supply a path. Egress follows the
Nexus rules: credentials are sent only to the origin of the URL and a redirect to
another host is not followed.

API used: `GET /api/repositories` (key, type, package type),
`GET /api/repositories/{key}` (remote URL, virtual members and default
deployment repository, cache period, remote username), `GET /api/storageinfo`
(sizes and file counts, admin only, calculated periodically by Artifactory so it
can lag), `POST /api/search/aql` (`items.find({repo,type:file})` with path, name, size
and sha256, paged with `.offset().limit()`), downloads from `/<repo>/<path>`, the
Docker v2 API under `/api/docker/<repo>/v2/` (catalog, tags, manifests, blobs), and
`/api/security/users|groups` and `/api/v2/security/permissions` for the report.

Mapping:

| Artifactory                      | Ant                                                                                                                                                      |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------- |
| local (and federated) repository | local source and a same-named single-source feed                                                                                                         |
| remote repository                | remote source with the same upstream URL and metadata TTL (retrieval cache period), plus a same-named feed; no artifacts are copied                      |
| virtual repository               | feed whose ordered sources are the virtual's `repositories` list (nested virtuals flattened); `defaultDeploymentRepo` becomes the publish policy default |

Package types map to Ant formats: docker, oci and helmoci to `oci`; maven and
gradle to `maven`; npm, pypi, go, cargo, nuget, helm, composer, conda, conan,
cran, pub, cocoapods, swift, terraform, ansible, rpm and p2 to the same name; gems
to `rubygems`; debian to `apt`; alpine to `apk`; gitlfs to `git-lfs`;
huggingfaceml to `huggingface`; generic to `raw`. Any other package type (bower,
chef, puppet, vagrant, ...) and any other repository type is listed as unmapped in
the report and never silently skipped. A remote or virtual repository whose
configuration cannot be read (reading it needs admin permission) is reported as
unmapped instead of being mapped incompletely.

What is copied, recreated or only reported:

- Recreated on every apply: sources, feeds, feed source order, publish
  policies, remote source origin and cache TTL.
- Copied only with `--copy-content --apply` (CLI), digest-verified and resumable:
  generic (raw) repositories, Maven and Gradle repositories (metadata files and
  snapshot versions are skipped as for Nexus) and Docker or OCI repositories
  (manifests, image indexes and their children, config and layer blobs, tags,
  subject/referrer links; schema 1 manifests are reported). Apply is idempotent:
  a second run reports everything unchanged, and a failed or tampered download is
  an integrity failure that leaves nothing behind and is picked up by the next run.
- Definitions only (content not copied, reported per repository): every other
  format, including npm and PyPI.
- Reported, never migrated: remote repository credentials (Artifactory never
  returns the stored password, so a remote with a username is listed as a
  credential to re-enter with `PUT /api/v1/sources/{id}/remote-credential`), users,
  groups and permission targets (with guidance to use OIDC/SAML identity bindings
  and scoped service tokens), outbound proxy, client TLS certificate and offline
  settings of remotes, `maxUniqueSnapshots` and `maxUniqueTags`, federation members.

Limits: Artifactory resolves local repositories before remote ones in a virtual
repository; Ant resolves in the listed order, so a virtual that lists a remote
before a local resolves differently. Artifactory has no cleanup-policy API that
Ant reads, so retention is not carried over. `ant compare` reads one path per line
through the same download URL as a client would.

## Management API and console

`POST /api/v1/transfer/export`, `POST /api/v1/transfer/import` (dry run by
default), `POST /api/v1/migrations/nexus`, `POST /api/v1/migrations/artifactory`,
`POST /api/v1/migrations/compare` (field `manager`: `nexus` or `artifactory`)
and `GET /api/v1/transfer/runs` require instance admin. The API handles
definitions; content bytes go through the CLI. Run history keeps counts only.
The console screen "Import & migrate" drives the same endpoints. Its Migrate tab is a
single screen with a Nexus | Artifactory switcher: a connection column (URL,
credentials, CA reference under Advanced), a table of the source's repositories
(format, type, mapped Ant feed or source, size, notes, include checkbox) that
scrolls on its own, and an action bar with the selection summary, Compare, Dry run
and Migrate. The dry-run report opens in a drawer, and Migrate stays disabled until
a dry run of the current selection has been made. Content copy is a CLI operation.
