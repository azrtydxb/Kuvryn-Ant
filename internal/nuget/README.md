# NuGet adapter

Implemented scope: V3 service discovery, flat-container version lists/package/nuspec downloads, registration indexes and leaves, and multipart V2 package push. Registration pages contain up to 64 inline leaves. All three assets (`nupkg`, `nuspec`, `metadata`) commit atomically through `artifact.Service.PublishBundle` with the same pinned local source. Identical retries succeed; differing bytes at the same normalized identity conflict.

## Integration

`cmd/ant/artifacts.go` mounts the adapter at `/feeds/{slug}/nuget/` and configures the origin. Construct `repository, err := nuget.New(db, service)`, call `repository.SetPublicURL(configuredOrigin)` before serving, and mount `/feeds/` routing for format `nuget`. The trusted origin should come from the parent's validated `ANT_PUBLIC_URL`. Only HTTPS or literal-loopback HTTP origins are accepted, without credentials/path/query/fragment. An unset origin returns 503. Request Host and forwarded headers never generate resource URLs. The handler binds to the service lease's instance.

Configure clients with `https://<origin>/feeds/<slug>/nuget/v3/index.json`. Push and delete use the token as `X-NuGet-ApiKey`, resolved by the shared client-authentication layer like Bearer (service tokens, PATs and GitHub JWTs; never a password login); reads accept Basic (token password) or Bearer, verified by the shared client-authentication layer (service tokens and PATs, as the feed's policy allows). Browser-context principals do not authenticate these routes. Service-index reads require feed read permission; every indexed/downloaded version passes shared artifact authorization. Authorization failures never reveal package metadata. Owner selection is per normalized package ID, without missing-version fallback. SQL result sets close before shared storage/policy calls, including with a one-connection pool.

## Bounds and validation

Uploads spool into mode-0600 temporary files and are deleted on every exit. Because ID/version reside inside ZIP metadata, the authenticated upload must be spooled and inspected before package-specific `Prepare`; bytes never reach permanent artifact storage before all three destinations are authorized and pinned. Concurrent uploads are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32). Limits are 512 MiB compressed, 2 GiB expanded, 10,000 entries, 8 MiB ZIP central directory, and 1 MiB nuspec. ZIP64 is rejected under these limits. Unsafe paths, duplicate case-insensitive names, symlinks, excessive compression ratios, corrupt checksums, multiple root nuspecs, filename/ID mismatches, XML directives/entities, duplicate identity elements, excessive XML nesting, and malformed identities fail closed. Archive reads check cancellation every 64 KiB and never execute content.

Package IDs and prerelease labels are lowercased; numeric components lose leading zeros; omitted components become zero; fourth-component zero and build metadata are removed. Nonzero fourth components remain supported. Numeric prerelease identifiers reject leading zeros. Registration versions use numeric/SemVer precedence rather than lexical sorting.

Indexes have a 15-second generation deadline, 4,096-version ceiling and 16 MiB encoded-document ceiling (15 MiB registration item budget); over-budget requests fail, never truncate or fall back. Transfer chunks are bounded to 64 KiB, and 30-second progress deadlines replace short control-server deadlines. Metadata reads are private/no-store.

## Executed evidence (2026-10-18)

`ANT_TEST_DATABASE_URL=<isolated local PostgreSQL> ANT_REQUIRE_INTEGRATION=1 go test -race -count=1 -v ./internal/nuget` passed against PostgreSQL 17.11, with a one-connection pool. Tests cover normalized collisions, atomic rollback after injected storage failure, Basic/Bearer/API-key auth, namespace denial, browser-principal rejection, foreign-instance rejection, trusted-origin/Host spoofing, unconfigured-origin failure, owner no-fallback, HEAD behavior, unsafe archives/XML, cancellation, and semantic version ordering.

The installed .NET SDK **8.0.131** successfully ran real `dotnet nuget push` followed by an isolated-cache `dotnet restore` of a fixture package. Executable SHA-256: `28bcb5281542ec643a631cb13c6b8cc420d7f6ba827dbe52301547656aff2308`. The native fixture uses a loopback read-credential bridge; actual endpoint credential handling is tested separately. It does not prove production TLS setup; the conformance harness runs the SDK against a feed directly.

Tests skip PostgreSQL when `ANT_TEST_DATABASE_URL` is absent unless `ANT_REQUIRE_INTEGRATION=1`; the native test skips if `dotnet` is absent. Only local disk storage was exercised, with no provider fleet.

## Delete, relist and remote sources

`DELETE v2/package/{id}/{version}` (`dotnet nuget delete`) removes the version through the core, gated by the feed's delete policy; when the policy only allows unlisting, or the release is immutable, it is unlisted instead (response header `X-Ant-Action: unlist`), and `POST v2/package/{id}/{version}` relists. With a remote source, the upstream service index is read through the metadata cache; the upstream PackageBaseAddress and RegistrationsBaseUrl resources are discovered from it, every registration document is re-rendered with `@id`, `packageContent` and registration URLs rebuilt from the feed base, registration pages that upstream does not inline are fetched and inlined (up to the 4,096 version ceiling), and `.nupkg`/`.nuspec` are cached on first download. NuGet publishes no sha256 for them, so these fills are length checked but not hash verified.

## Not implemented

Search and autocomplete, the V2 OData feed and Chocolatey discovery, and symbol packages (`.snupkg`). The service index advertises only implemented resources. The real .NET SDK (10.0.402 in the conformance harness) is exercised by `conformance/run.sh local nuget`.

Official protocol references used: [service API](https://learn.microsoft.com/en-us/nuget/api/overview), [push](https://learn.microsoft.com/en-us/nuget/api/package-publish-resource), [flat container](https://learn.microsoft.com/en-us/nuget/api/package-base-address-resource), [registration](https://learn.microsoft.com/en-us/nuget/api/registration-base-url-resource), and [version normalization](https://learn.microsoft.com/en-us/nuget/concepts/package-versioning#normalized-version-numbers). Repository requirements: `docs/planning/contracts/F15.md`, `common.md`, and `architecture-baseline.md`.
