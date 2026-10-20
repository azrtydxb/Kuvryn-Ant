# Hook actions

A hook runs exactly one action. Actions execute as the principal that **created the hook**, with that principal's current roles: if the creator loses the permission, or is disabled, the delivery fails with a clear result instead of acting with more rights.

| Type                 | What it does                                                                                            |
| -------------------- | ------------------------------------------------------------------------------------------------------- |
| `upstream_notify`    | Reads a registry or forge notification and invalidates the matching metadata cache; optionally prewarms |
| `prewarm`            | Prewarms a package version or image tag in a feed                                                       |
| `replication_run`    | Runs a replication job now                                                                              |
| `purge_source_cache` | Purges a remote source's cache, all of it or one coordinate                                             |
| `clean_cache`        | Cleans a [cache feed](../build-caches/) by prefix (the same call as `DELETE /feeds/{id}/cache`)         |
| `relay`              | Forwards the verified payload to an internal HTTPS URL                                                  |

## Templates

Name, version, coordinate and prefix fields are templates over the JSON payload. The language is deliberately tiny and cannot execute anything: literal text plus <code v-pre>{{ .path.to.value }}</code>, with array indexes as numbers and an optional filter chain.

```text
{{ .release.tag_name }}                    v1.2.3
{{ .release.tag_name | trimv }}            1.2.3
{{ .repository.full_name | lower }}        acme/tool
{{ .ref | base | trimv }}                  2.0.0   (from refs/tags/v2.0.0)
{{ .packages.0.version }}                  first array element
```

Filters: `lower`, `upper`, `trimv` (strips a leading `v` before a digit) and `base` (the part after the last `/`). There are no functions, conditionals, loops or variables. A path that is missing, empty, null, an object or an array fails the delivery (`template_error`, not retried), so an action never runs with an empty name. Results are limited to 1024 bytes. When you save a hook, the templates are rendered against a sample payload and the resulting item is validated for the feed's format.

## Upstream change notifications

`upstream_notify` understands these payloads. Set `provider` or leave it on `auto`.

| Provider      | Events used                                                    | Identifies                                 | Sources matched by                                                                |
| ------------- | -------------------------------------------------------------- | ------------------------------------------ | --------------------------------------------------------------------------------- |
| `dockerhub`   | push                                                           | `repository.repo_name`, `push_data.tag`    | origin Docker Hub (`registry-1.docker.io` and aliases)                            |
| `github`      | `release`, `package`, `registry_package` (`ping` is ignored)   | repository or package, tag or version      | origin `github.com`, `ghcr.io` or the `*.pkg.github.com` host of the package type |
| `gitlab`      | Push, Tag Push, Release                                        | `project.path_with_namespace`, branch, tag | host of `project.web_url` (and `registry.gitlab.com` on gitlab.com)               |
| `npm`         | `package:publish` and other package events (stars are ignored) | package name, version                      | origin `registry.npmjs.org`                                                       |
| `artifactory` | `artifact` and `docker` domains                                | path, image name, tag                      | the sources or origins you configure                                              |
| `nexus`       | component and asset events                                     | group, name, version                       | the sources or origins you configure                                              |

Artifactory and Nexus payloads do not name their server, so for those you list the **remote sources** (`source_ids`) or **origins** (host names) the hook covers; saving is refused without one. For any provider you may also list `source_ids` or `origins` to override the automatic match. A container change only applies to OCI sources and any other change only to non-OCI sources, unless sources are named explicitly.

For every matched source, Ant drops the cached **metadata** and **negative-cache** entries of that package (tag lists, version indexes, "not found" answers), so new tags and versions are visible immediately. Immutable artifacts already cached are untouched. With `prewarm` enabled, the changed tag or version is also prewarmed through the feeds that use the source (optionally only `feed_id`). The delivery result shows the matched sources, the entries dropped and the prewarm runs started. A payload that matches no source is a quiet success (`no_matching_source`), not an error.

The metadata cache is in the memory of the serving process, so invalidation applies to the instance that holds the writer lease and serves the traffic.

## Prewarm

```json
{
  "type": "prewarm",
  "feed_id": "<FEED_ID>",
  "kind": "package",
  "name": "{{ .repository.full_name | lower }}",
  "version": "{{ .release.tag_name | trimv }}"
}
```

`kind` is `package` or `image` (for an OCI feed the name is the repository and the version a tag or digest). Each delivery creates a one-off prewarm job named `hook:<slug>:<delivery>` and queues its run, so the result is visible under Settings, Cache prewarm. These jobs are removed with the delivery log.

## Replication, purge and cache clean

```json
{ "type": "replication_run", "job_id": "<REPLICATION_JOB_ID>" }
{ "type": "purge_source_cache", "source_id": "<REMOTE_SOURCE_ID>", "coordinate": "{{ .package }}" }
{ "type": "clean_cache", "feed_id": "<CACHE_FEED_ID>", "prefix": "{{ .repository }}:pr-{{ .number }}" }
```

An empty `coordinate` purges the whole cache of the source. Freed blobs are reclaimed by the garbage collector, never inline. `clean_cache` requires the creator to hold delete on the feed and applies the feed's normal rules.

## Relay

```json
{
  "type": "relay",
  "target_url": "https://ci-gateway.example.com/hooks/github",
  "resign_secret_id": "<SIGNING_SECRET_ID>"
}
```

The verified body is forwarded unchanged to the target, through the same hardened transport as outgoing webhooks: https only, no proxies, no redirects, special-use addresses refused on every connection. Delivery uses the same retry schedule (up to 8 attempts with exponential backoff) and the delivery log shows each attempt.

- **Headers.** Only a safe allowlist is forwarded: `Content-Type`, `User-Agent`, the provider event and delivery identifiers (`X-GitHub-*`, `X-Gitlab-Event*`, `X-Event-Key`, `X-Nexus-Webhook-*`) and Ant's `X-Ant-Hook` and `X-Ant-Delivery`. `Authorization`, `X-Gitlab-Token`, cookies and forwarding headers are never forwarded.
- **Signature.** Without `resign_secret_id` the original body signature header (`X-Hub-Signature-256`, `X-NPM-Signature`) is passed through, so a target that holds the sender's secret can verify it. With it, the payload is re-signed with the chosen signing secret using the outgoing webhook scheme (`X-Ant-Event-ID`, `X-Ant-Timestamp`, `X-Ant-Signature` = HMAC-SHA256 over timestamp, event id and body joined by newlines; see [Outgoing webhooks](./outgoing#verifying-a-delivery)).
