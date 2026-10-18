# Local publication, identity and client evidence

Implementation continues on `feat/service-foundation` and PR223. This record describes working increments, not a Phase1 release or completion of the full backlog.

## Implemented runtime

After bootstrap, the serving instance holds a renewable PostgreSQL writer lease. Another process cannot acquire the same live instance; lost ownership stops serving. Shutdown retains ownership until HTTP drain finishes. Every artifact metadata transaction checks the current epoch while holding the lease row.

The management API now configures sources, ordered feed attachments, explicit local publish policies, principals, roles, grants, tokens and immutable external identity bindings. Configuration writes use revision checks and replay-safe results; token issuance returns a secret once and does not cache it. Each local source has an isolated deduplication domain.

Publication stages and verifies content before atomic metadata visibility. Bundles commit all assets together, with reference closure checks, provenance and transactional audit/outbox. Identical retries do not duplicate events; conflicting immutable content is rejected. Failed metadata commits can leave unreferenced verified bytes; reference-safe garbage collection remains outstanding. Reads preserve package ownership rather than silently mixing sources.

Storage can use local disk or S3 with mounted credentials and custom CA roots. Shared upload admission bounds S3 part buffers across domains. Persistent local upload staging is required even with S3. No actual MinIO or AWS compatibility certification is claimed by SDK fixtures.

## Client evidence

Local disposable PostgreSQL17 fixtures and installed clients exercised:

| Workflow                                                                 | Actual client          | Evidence                                |
| ------------------------------------------------------------------------ | ---------------------- | --------------------------------------- |
| OCI image push/pull, digest checks, restart/resume, multi-platform index | crane v0.21.6          | `scripts/ocismoke`                      |
| Go module ingestion and native download                                  | Go1.27.1               | `internal/goproxy` PostgreSQL tests     |
| Classic chart index, pull and dependency resolution                      | Helm v4.1.1            | `internal/helmrepo` PostgreSQL tests    |
| Crate publication, fetch and offline build                               | Cargo1.97.1            | `internal/cargo` PostgreSQL tests       |
| Raw immutable upload, conditions/ranges and isolation                    | Go HTTP fixtures       | `internal/rawrepo` PostgreSQL tests     |
| npm publish, view, initial dist-tags, install and offline CI             | npm10.9.8              | `internal/npmregistry` PostgreSQL tests |
| Python hash-verified download/install and requires-python rejection      | pip26.2.1              | `internal/pypi` PostgreSQL tests        |
| Maven release parent-POM resolution                                      | Maven3.9.16            | `internal/maven` PostgreSQL tests       |
| NuGet push and restore                                                   | .NET8.0.131            | `internal/nuget` PostgreSQL tests       |
| Git LFS object push/pull and byte-range reads                            | Git HTTP fixtures      | `internal/gitlfs` PostgreSQL tests      |
| Composer install, locked install and package metadata                    | Composer2.9.7/PHP8.5.6 | `internal/composer` PostgreSQL tests    |
| Pub version fetch, archive install and offline checks                    | Dart fixtures          | `internal/pubrepo` PostgreSQL tests     |
| RubyGems push, compact index, install and checksums                      | RubyGems fixtures      | `internal/rubygems` PostgreSQL tests    |

Conan, ansible-galaxy, conda and CRAN adapters are implemented and mounted with
boundary-tested local fixtures (`internal/conan`, `internal/ansiblerepo`,
`internal/condarepo`, `internal/cran`); native-client evidence for those four
formats and remote/replica resolution remain release gates. The remote-source
runtime (`internal/remoteruntime`) materializes authorized remote config and
credentials, applies CAs and redirect policy, and streams/caches metadata and
archives with per-source quotas under `scripts/test-postgres.sh` fixtures.
Pull-through reads (remote materialization into the artifact store, coalesced
misses, negative caching, stale-if-error) are wired for raw, npm, pypi, maven, go
and OCI; see [pull-through.md](pull-through.md) for semantics, limits and how to
enable the remaining adapters. Replica resolution remains a release gate.

These actual versions differ from some planned release-matrix pins. Remote/replica behavior, deployment-scale workloads, mutable lifecycle policies and full format certification remain separate gates. Inspect each adapter's README for its exact supported operations and explicit limits.

## Identity and console

OIDC code/PKCE uses reviewed token-verification libraries, exact trusted endpoints, one-time browser-bound state, explicit issuer/subject bindings and PostgreSQL sessions. SAML uses reviewed signature verification, browser correlation and instance/provider-scoped PostgreSQL replay claims. LDAP requires verified LDAPS or StartTLS and bounded group traversal. Browser sessions use secure HttpOnly cookies, CSRF checks and current principal/binding checks. Provider callbacks return to the console. LDAP group suggestions do not implicitly grant administrator roles.

Tests use signed provider fixtures and local TLS fixtures. Actual Zitadel, Keycloak, OpenLDAP and Windows AD acceptance remains outstanding. SAML local logout revokes the browser session; provider-wide signed logout is not yet exposed by browser routes.

The React console configures implemented management resources; no bearer token is persisted in browser storage. Server sessions are the production browser-authentication path. Format configuration choices do not claim that all26 protocols are implemented.

## Review corrections

The runtime now pins Go1.27.2 and goxmldsig1.6.0. The latter addresses [GO-2026-4753](https://pkg.go.dev/vuln/GO-2026-4753); SAML signature fixtures also enforce the supported single-reference structure. Forced shutdown joins handler cleanup before closing storage. Background delivery retains an immutable instance scope and rejects startup without matching writer ownership.

Root encryption keys support `ANT_ROOT_KEY_VERSION` (default `v1`) and an optional `ANT_PREVIOUS_ROOT_KEYS_FILE`, a bounded JSON map from earlier version names to mounted 32-byte key files. Keep earlier keys available while encrypted records still reference them. Run the local `ant rotate-secrets` command with both active and previous mounted keys to rewrap at most100 records per invocation. The command emits only a count and batch limit and can be safely resumed; it does not issue an access token. Keep previous keys until no stored records reference their versions (a zero batch can also mean rows are currently locked by another rotation).

## Remaining work

The entire Phase1 scope remains required: the remaining format adapters and certification, replication, retention/GC, backups and restore, provider administration, release packaging/qualification and deployment evidence. OKE plus existing MinIO on TrueNAS SCALE is the primary deployment target. AWS and other compatible providers remain supported targets without a separate environment fleet. Licensing remains deliberately undecided.
