# NuGet adapter

Implemented scope: authenticated local V3 service discovery, flat-container version lists/package/nuspec downloads, registration indexes and leaves, and multipart V2 package push. Registration pages contain up to 64 inline leaves. All three assets (`nupkg`, `nuspec`, `metadata`) commit atomically through `artifact.Service.PublishBundle` with the same pinned local source. Identical retries succeed; differing bytes at the same normalized identity conflict.

## Parent integration

No new module dependencies, migrations, main changes, or OpenAPI changes are needed by this package. Construct `repository, err := nuget.New(db, service)`, call `repository.SetPublicURL(configuredOrigin)` before serving, and mount `/feeds/` routing for format `nuget`. The trusted origin should come from the parent's validated `ANT_PUBLIC_URL`. Only HTTPS or literal-loopback HTTP origins are accepted, without credentials/path/query/fragment. An unset origin returns 503. Request Host and forwarded headers never generate resource URLs. The handler binds to the service lease's instance.

Configure clients with `https://<origin>/feeds/<slug>/nuget/v3/index.json`. Push uses the shared service-token secret as `X-NuGet-ApiKey`; reads accept Basic (token password) or Bearer. Browser-context principals do not authenticate these routes. Service-index reads require feed read permission; every indexed/downloaded version passes shared artifact authorization. Authorization failures never reveal package metadata. Owner selection is per normalized package ID, without missing-version fallback. SQL result sets close before shared storage/policy calls, including with a one-connection pool.

## Bounds and validation

Uploads spool into mode-0600 temporary files and are deleted on every exit. Because ID/version reside inside ZIP metadata, the authenticated upload must be spooled and inspected before package-specific `Prepare`; bytes never reach permanent artifact storage before all three destinations are authorized and pinned. Four uploads can spool concurrently. Limits are 512 MiB compressed, 2 GiB expanded, 10,000 entries, 8 MiB ZIP central directory, and 1 MiB nuspec. ZIP64 is rejected under these limits. Unsafe paths, duplicate case-insensitive names, symlinks, excessive compression ratios, corrupt checksums, multiple root nuspecs, filename/ID mismatches, XML directives/entities, duplicate identity elements, excessive XML nesting, and malformed identities fail closed. Archive reads check cancellation every 64 KiB and never execute content.

Package IDs and prerelease labels are lowercased; numeric components lose leading zeros; omitted components become zero; fourth-component zero and build metadata are removed. Nonzero fourth components remain supported. Numeric prerelease identifiers reject leading zeros. Registration versions use numeric/SemVer precedence rather than lexical sorting.

Indexes have a 15-second generation deadline, 4,096-version ceiling and 16 MiB encoded-document ceiling (15 MiB registration item budget); over-budget requests fail, never truncate or fall back. Transfer chunks are bounded to 64 KiB, and 30-second progress deadlines replace short control-server deadlines. Metadata reads are private/no-store.

## Executed evidence (2026-10-18)

`ANT_TEST_DATABASE_URL=<isolated local PostgreSQL> ANT_REQUIRE_INTEGRATION=1 go test -race -count=1 -v ./internal/nuget` passed against PostgreSQL 17.11, with a one-connection pool. Tests cover normalized collisions, atomic rollback after injected storage failure, Basic/Bearer/API-key auth, namespace denial, browser-principal rejection, foreign-instance rejection, trusted-origin/Host spoofing, unconfigured-origin failure, owner no-fallback, HEAD behavior, unsafe archives/XML, cancellation, and semantic version ordering.

The installed .NET SDK **8.0.131** successfully ran real `dotnet nuget push` followed by an isolated-cache `dotnet restore` of a fixture package. Executable SHA-256: `28bcb5281542ec643a631cb13c6b8cc420d7f6ba827dbe52301547656aff2308`. The native fixture uses a loopback read-credential bridge; actual endpoint credential handling is tested separately. It does not prove production TLS setup or the contract's pinned .NET 10.0.402 baseline.

Tests skip PostgreSQL when `ANT_TEST_DATABASE_URL` is absent unless `ANT_REQUIRE_INTEGRATION=1`; the native test skips if `dotnet` is absent. Only local disk storage was exercised, with no provider fleet.

## Remaining contract scope

This is not full F15 conformance. Search, V2 OData/Chocolatey discovery, symbols/PDB workflows, delete/unlist, upstream hydration, replication/offline closure, protocol-specific lifecycle diagnostics, and the pinned client baselines remain outside this implementation. The service index advertises only implemented resources. The current hard resource limits require configurable policy work before broader workload claims.

Official protocol references used: [service API](https://learn.microsoft.com/en-us/nuget/api/overview), [push](https://learn.microsoft.com/en-us/nuget/api/package-publish-resource), [flat container](https://learn.microsoft.com/en-us/nuget/api/package-base-address-resource), [registration](https://learn.microsoft.com/en-us/nuget/api/registration-base-url-resource), and [version normalization](https://learn.microsoft.com/en-us/nuget/concepts/package-versioning#normalized-version-numbers). Repository requirements: `docs/planning/contracts/F15.md`, `common.md`, and `architecture-baseline.md`.
