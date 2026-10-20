# Import, export and migration from Nexus or Artifactory

Backups (`ant backup`) copy a whole deployment. The transfer tooling on this page moves **selected feeds** between Kuvryn Ant instances, and brings repositories in from Sonatype Nexus Repository 3 or JFrog Artifactory for a staged adoption.

All commands are local operations, authorized by possession of the deployment's database and storage configuration (like `ant backup`), and use the same environment as `ant serve`. `ant import` and `ant migrate` only report (dry run) unless `--apply` is given.

```sh
ant export [--feed SLUG]... [--namespace NS]... [--no-content] <new-or-empty-dir>
ant export verify [--expect-manifest-sha256 HEX] <dir>
ant import [--apply] [--expect-manifest-sha256 HEX] <dir>
ant migrate nexus|artifactory --url URL [--user NAME] [--ca-ref ID] [--repo NAME]... [--copy-content] [--no-count] [--apply]
ant compare nexus|artifactory --url URL --coordinates FILE
```

## Export bundles

```sh
ant export --feed npm --feed pypi /srv/transfer/npm-pypi-2026-10-20
ant export verify --expect-manifest-sha256 <hex> /srv/transfer/npm-pypi-2026-10-20
```

| Flag             | Meaning                                                                                                     |
| ---------------- | ----------------------------------------------------------------------------------------------------------- |
| `--feed SLUG`    | Export this feed; repeatable. All feeds when none is named                                                  |
| `--namespace NS` | Limit the exported content to a namespace (matches itself and children separated by `/` or `.`); repeatable |
| `--no-content`   | Definitions and inventory only, no blob bytes                                                               |

A bundle directory contains:

| Path             | Content                                                                                                                                                                                                                    |
| ---------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `document.json`  | Feeds, sources, ordered feed sources, the anonymous-read flag, publish policies, retention policies, remote source configuration, and the inventory of local content with checksums, paths, namespaces and format metadata |
| `blobs/<sha256>` | Content bytes (unless `--no-content`)                                                                                                                                                                                      |
| `manifest.json`  | Written last; digests of the document and every blob                                                                                                                                                                       |

Record the manifest digest printed by `ant export` and pass it back with `--expect-manifest-sha256` to detect replacement of a whole bundle. `ant export verify` needs no database.

**Never exported:** credential values, root keys, token secrets. A remote source carries only a `credential_ref`; after import, the report lists it under "unmapped" and you provision the secret with `PUT /api/v1/sources/{id}/remote-credential`. Policy selectors that name a principal ID are dropped (IDs are not portable) and reported.

Remote caches and replica sources export definitions only; their content is re-fetched or re-replicated. Replica sources are not imported, because they need a replication peer (see [Replication](../replication/index.md)).

## Import bundles

```sh
ant import --expect-manifest-sha256 <hex> /srv/transfer/npm-pypi-2026-10-20          # dry run
ant import --apply --expect-manifest-sha256 <hex> /srv/transfer/npm-pypi-2026-10-20  # apply
```

The dry run executes the same code as the apply inside a rolled-back transaction, so its report is exactly what the apply will do: per object `created`, `updated`, `unchanged`, `skipped` or `conflict`; unmapped configuration; integrity failures.

- Apply is idempotent: running it again reports everything unchanged.
- Existing content is never overwritten. A different digest at an existing path is a conflict.
- A blob that fails digest or size verification is reported and its asset skipped.
- The command exits with status **2** when integrity failures are found (status 1 for other errors).

## Migrating from Nexus Repository 3

The migrator inventories a Nexus 3 instance through its REST API and maps it onto feeds and sources.

Credentials are read from the environment at run time and never stored:

| Variable             | Use                                                                  |
| -------------------- | -------------------------------------------------------------------- |
| `ANT_NEXUS_PASSWORD` | Password for `--user` (HTTP Basic)                                   |
| `ANT_NEXUS_TOKEN`    | Sent as `Authorization: Bearer <token>` instead of Basic credentials |

They are only ever sent to the origin of `--url`.

```sh
export ANT_NEXUS_PASSWORD='<password>'      # or: export ANT_NEXUS_TOKEN='<token>'
# 1. Dry run: inventory and mapping report, nothing written
ant migrate nexus --url https://nexus.example.com --user admin
# 2. Limit to some repositories, skip asset counting on large instances
ant migrate nexus --url https://nexus.example.com --user admin --repo maven-releases --repo npm-proxy --no-count
# 3. Apply definitions and copy hosted content
ant migrate nexus --url https://nexus.example.com --user admin --copy-content --apply
```

| Flag             | Meaning                                                         |
| ---------------- | --------------------------------------------------------------- |
| `--url URL`      | Nexus base URL (required)                                       |
| `--user NAME`    | User for Basic authentication                                   |
| `--repo NAME`    | Limit to these repositories; repeatable                         |
| `--copy-content` | Also download and store the assets of hosted repositories       |
| `--no-count`     | Do not count assets per repository in the report                |
| `--apply`        | Write the mapped configuration (and content); otherwise dry run |

### Mapping

| Nexus                           | Kuvryn Ant                                                                                                                                         |
| ------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| Hosted repository               | Local source, plus a same-named single-source feed                                                                                                 |
| Proxy repository                | Remote source (origin, metadata TTL), plus a same-named single-source feed. An authenticated proxy is reported so its credential can be re-entered |
| Group repository                | Feed with its members as ordered sources; nested groups are flattened and the writable member becomes the publish default                          |
| Cleanup policy: last updated    | Retention `max_age`                                                                                                                                |
| Cleanup policy: last downloaded | Retention `max_idle`                                                                                                                               |
| Cleanup policy: retain N        | Retention `keep_last`                                                                                                                              |

The same-named feeds let you keep client URLs stable per repository. Regex and prerelease-only cleanup criteria, and additional policies, are reported as unmapped.

`--copy-content` downloads the assets of hosted repositories through their download URLs and verifies each against Nexus's SHA-256 before it is stored. Maven metadata files are not copied, because Kuvryn Ant generates them. Like `ant import`, the command exits 2 on integrity failures.

::: warning Limits

- Content copy supports **raw and Maven layouts only**. Other formats are mapped as definitions and reported; their content must be republished or re-fetched.
- Nexus Repository 3 and Artifactory are implemented. Other managers need an implementation of the `Manager` interface in [internal/transfer/migrate.go](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/transfer/migrate.go).
- The migration has been exercised against synthetic fixtures; a migration of a real production Nexus instance has not yet been qualified.
  :::

## Migrating from Artifactory

The migrator inventories a JFrog Artifactory instance through its REST API (repositories, per-repository configuration, the storage summary, AQL for assets, the Docker v2 API for registries, and the security endpoints for the report) and maps it onto feeds and sources.

Credentials are read from the environment at run time and never stored:

| Variable                   | Use                                                   |
| -------------------------- | ----------------------------------------------------- |
| `ANT_ARTIFACTORY_TOKEN`    | Access token, sent as `Authorization: Bearer <token>` |
| `ANT_ARTIFACTORY_PASSWORD` | Password or API key for `--user` (HTTP Basic)         |

They are only ever sent to the origin of `--url`, and a redirect to another host is not followed. `--ca-ref ID` selects a CA bundle for a private certificate authority from the operator's `ANT_UPSTREAM_CA_MAP_FILE`; a request can never supply a file path. `/artifactory` is appended when the URL has no path.

```sh
export ANT_ARTIFACTORY_TOKEN='<access token>'
# 1. Dry run: inventory and mapping report, nothing written
ant migrate artifactory --url https://example.jfrog.io/artifactory
# 2. Limit to some repositories (members of a selected virtual repository are pulled in)
ant migrate artifactory --url https://example.jfrog.io/artifactory --repo libs-all --repo docker-local
# 3. Apply definitions and copy local content
ant migrate artifactory --url https://example.jfrog.io/artifactory --copy-content --apply
```

### Mapping

| Artifactory                      | Kuvryn Ant                                                                                                                                                                                                    |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Local (and federated) repository | Local source, plus a same-named single-source feed                                                                                                                                                            |
| Remote repository                | Remote source with the same upstream URL and metadata TTL (retrieval cache period), plus a same-named feed. No artifacts are copied; a remote with a username is reported so its credential can be re-entered |
| Virtual repository               | Feed whose ordered sources are the virtual repository list (nested virtuals flattened); the default deployment repository becomes the publish default                                                         |

Package types map to Ant formats: docker, oci and helmoci to oci; maven and gradle to maven; npm, pypi, go, cargo, nuget, helm, composer, conda, conan, cran, pub, cocoapods, swift, terraform, ansible, rpm and p2 to the same name; gems to rubygems; debian to apt; alpine to apk; gitlfs to git-lfs; huggingfaceml to huggingface; generic to raw. Any other package type or repository type is listed as unmapped, never silently skipped. A remote or virtual repository whose configuration cannot be read (it needs admin permission) is reported as unmapped instead of being mapped incompletely.

| Handling                     | What                                                                                                                                                                                                                                               |
| ---------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Recreated on every apply     | Sources, feeds, feed source order, publish policies, remote origin and cache TTL                                                                                                                                                                   |
| Copied with `--copy-content` | Generic (raw), Maven and Gradle, and Docker or OCI repositories (manifests, indexes and children, config and layer blobs, tags, referrers). Each object is SHA-256 verified; apply is idempotent and a failed run resumes where it stopped         |
| Definitions only             | Every other format, including npm and PyPI; republish or re-fetch the content                                                                                                                                                                      |
| Reported, never migrated     | Remote repository credentials, users, groups and permission targets (use OIDC or SAML identity bindings and scoped service tokens), outbound proxy and client certificate of remotes, `maxUniqueSnapshots` and `maxUniqueTags`, federation members |

Sizes in the report come from the Artifactory storage summary (admin only; Artifactory refreshes it periodically, so it can lag) and need no asset listing. Artifactory resolves local repositories before remote ones inside a virtual repository, while Ant resolves in the listed order. Artifactory has no cleanup-policy API that Ant reads, so retention is not carried over.

## Staged adoption with `ant compare`

Before switching clients, compare what they would receive from Nexus and from Kuvryn Ant. The coordinates file has one line per coordinate: `<repository> <path> [<feed>]` (blank lines and lines starting with `#` are ignored; at most 4 MiB).

```text
# coords.txt
maven-releases com/example/app/1.2.0/app-1.2.0.jar maven
raw-hosted tools/installer.sh
```

```sh
ant compare nexus --url https://nexus.example.com --user admin --coordinates coords.txt
ant compare artifactory --url https://example.jfrog.io/artifactory --coordinates coords.txt
```

When `<feed>` is omitted, the feed slug derived from the repository name is used (the same name the migrator gives the single-source feed). For each coordinate the report shows the SHA-256 from Nexus and from the Ant feed (the first source in feed order that stores the path) and a status: `identical`, `differs`, `missing_in_ant`, `missing_in_source`, `missing_in_both`, or `error` when Nexus could not be queried. It is read-only.

::: tip
Only content already stored in the feed's sources is compared. A path that a remote source would fetch on demand is reported as missing in Ant until it has been fetched once (or [prewarmed](./prewarm.md)). The path is matched against the stored asset path, which is a natural fit for raw and Maven layouts.
:::

## Management API and console

| Endpoint                              | Purpose                                                                                                                                      |
| ------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| `POST /api/v1/transfer/export`        | Return the portable definition document of the selected feeds (optionally with the content inventory, not bytes)                             |
| `POST /api/v1/transfer/import`        | Import a definition document; `dry_run` defaults to `true`. Assets are imported only when their content already exists in the target store   |
| `POST /api/v1/migrations/nexus`       | Inventory and map a Nexus instance; credentials are used for this request only; `dry_run` defaults to `true`; applying maps definitions only |
| `POST /api/v1/migrations/artifactory` | Same for Artifactory; also accepts `ca_ref`                                                                                                  |
| `POST /api/v1/migrations/compare`     | Staged-adoption comparison; `manager` is `nexus` (default) or `artifactory`                                                                  |
| `GET /api/v1/transfer/runs`           | Run history (counts only)                                                                                                                    |

All require instance admin. The API handles definitions; content bytes go through the CLI (`ant export`/`ant import`, `ant migrate nexus --copy-content --apply`). When the server calls Nexus it connects to the supplied URL and does not follow redirects to other hosts. The console screen **Import & migrate** drives the same endpoints. Its **Migrate** tab is a single screen with a Nexus | Artifactory switcher: a connection column, a table of the source's repositories (format, type, mapped feed or source, size, notes, include checkbox) that scrolls on its own, and an action bar with the selection summary, Compare, Dry run and Migrate. The dry-run report opens in a drawer, and Migrate stays disabled until a dry run of the current selection exists. Content copy stays a CLI operation.
