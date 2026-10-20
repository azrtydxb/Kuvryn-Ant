# FAQ

Short, practical answers to the questions operators and developers ask most often. Each answer links to the page with the details.

## Is Kuvryn Ant production ready?

Not as a supported release. The current version is **0.1.0**, and there is no published release image, tagged release or release notes yet; you build the image from the repository's `Dockerfile`. All 26 format adapters, remote pull-through, replication, federation, the console, the management API and MCP are implemented and run in the maintainers' own deployments, but several release gates are still open: a fresh conformance run across all formats, performance targets, real-provider identity evidence, and restore/rollback re-runs. See the [Roadmap](./roadmap.md) for the list. Evaluate it, run it for build caches and internal mirrors, and keep backups; do not treat it as a vendor-supported product.

## Can I run more than one replica for high availability?

No, not as active-active. Each instance has exactly **one writer**: a database writer lease ensures only one process serves writes and runs background workers, and the Helm chart deploys a single replica with the `Recreate` strategy, so an upgrade has a short downtime. For availability across sites, run independent instances and use [replication](../replication/index.md) (read-only replicas of selected feeds) or [federation](../replication/federation.md). A replica keeps serving its content when the source site is down.

## Which database does it need?

**PostgreSQL 17 or 18**, for every installation including development. There is no embedded database mode. All metadata lives in PostgreSQL; blobs live on disk or in S3-compatible object storage. See [PostgreSQL](../install/postgresql.md).

## Does it support S3?

Yes. Blobs can be stored on local disk or in any S3-compatible object store (SigV4, custom endpoint, region, bucket, prefix, path-style addressing, custom CA). The backend-equivalence test suite passes on local disk, MinIO and Oracle Cloud Object Storage's S3 interface; other providers such as AWS S3 use the same configuration but have not been certified. You can move between disk and S3 while serving with [storage migration](../operations/storage-migration.md). Upload staging always stays on the local data volume. See [Blob storage](../install/storage.md).

## What licence is it under?

The [Apache License 2.0](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/LICENSE). There are no paid editions or feature gates.

## How is pull-through different from mirroring?

|                | Pull-through (remote source)                  | Replication (replica source)                                     | Prewarm                                                          |
| -------------- | --------------------------------------------- | ---------------------------------------------------------------- | ---------------------------------------------------------------- |
| What it copies | Only what clients request                     | A whole feed (or filtered part) from another Kuvryn Ant instance | A list you choose: packages, a lockfile, images, or another feed |
| When           | On first request, then cached                 | Seed, then incremental, on a schedule or continuously            | Now or on a schedule                                             |
| Upstream       | Any public or private registry of that format | Another Kuvryn Ant instance only                                 | Through the feed's remote source                                 |
| Writable       | No (cache)                                    | No (read-only replica)                                           | No                                                               |

A **remote source** fetches an artifact from the upstream the first time a client asks for it, verifies it and stores it; later requests are local hits. Mutable metadata (indexes, tag lists) is cached with a TTL. **Replication** copies feeds between Kuvryn Ant instances, verifying every digest. [Prewarm](../operations/prewarm.md) fills a pull-through cache ahead of time. For container hosts, the OCI adapter can also act as a registry mirror for containerd, CRI-O, Docker and Podman (see [Host-level caching](../host-caching/index.md)).

## What happens when an upstream registry is down?

- Artifacts that were already fetched are served from local storage as usual.
- Mutable metadata is served from the metadata cache within its `stale_if_error` window; such responses carry `X-Kuvryn-Cache: stale` (and `Warning: 110`). When the upstream is unreachable and the adapter falls back to the locally cached view, the response is marked `X-Kuvryn-Cache: offline`.
- Content never fetched before cannot be served; the client gets an error (503), not a wrong answer.
- An upstream error stops the search in a feed's read order: Kuvryn Ant does not fall through to a lower-priority public source when a private one is failing, so an outage cannot cause dependency confusion.

See [Fallback behaviour](../host-caching/fallback.md) and [Read order and resolve explain](../concepts/resolution.md).

## Can it run fully offline (air-gapped)?

Yes, as long as the content is already inside. Local sources need no network. Fill caches beforehand with [prewarm](../operations/prewarm.md), move feeds between instances with [export and import bundles](../operations/import-export.md), or replicate from a connected instance. Offline mode is not a switch: remote sources simply serve what they have cached and report errors for the rest.

## Does it scan for vulnerabilities?

No. Kuvryn Ant has no scanning engine. External scanners can attach their verdict to an asset with `POST /api/v1/assets/{id}/security-results` (`passed`, `failed` or `warning` plus a free-form summary), which is stored with the asset and emitted as an `asset.security_result` webhook event. Results are informational: they do not block downloads. See [Comparison](./comparison.md).

## How do clients authenticate?

With a service-account token or a personal access token, sent either as a bearer token or as the password of HTTP Basic authentication (any username). GitHub Actions can exchange its OIDC token for a short-lived scoped token. See [Package client auth](../identity/client-auth.md) and [GitHub Actions](../ci/github-actions.md).

## Can I migrate from Nexus?

Partly. `ant migrate nexus` maps Nexus Repository 3 hosted, proxy and group repositories and cleanup policies onto sources, feeds and retention policies, and can copy hosted content for raw and Maven layouts; `ant compare nexus` checks that clients would get identical bytes. Other formats are mapped as definitions only, and other managers are not supported. See [Import, export, Nexus migration](../operations/import-export.md).

## How do I report a bug or ask for a feature?

Open an issue on [GitHub](https://github.com/azrtydxb/Kuvryn-Ant/issues). Include the version or commit, the format and client version, the request and response (with `X-Request-ID`), and the relevant log lines. For server-side problems, attach a [diagnostics bundle](../operations/diagnostics.md#diagnostics-bundle) after reviewing it. Report security vulnerabilities privately as described in [SECURITY.md](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/SECURITY.md), never in a public issue.
