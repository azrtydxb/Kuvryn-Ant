# Observability

Kuvryn Ant exposes Prometheus metrics, structured JSON logs, optional OpenTelemetry traces, a durable audit log with optional SIEM streaming, and a redacted diagnostics bundle (see [Diagnostics and repair](./diagnostics.md)). None of the telemetry records request paths, query strings, credentials, principals or resource IDs as labels or attributes.

## Metrics endpoint

`GET /metrics` serves the Prometheus text format (`text/plain; version=0.0.4`) on the application port (8080 in the chart).

::: warning Restrict access to /metrics
The endpoint has no authentication of its own. The chart's Ingress and HTTPRoute do not route `/metrics` (they route only `/`, `/console`, `/api`, `/auth`, `/v2` and `/feeds`). Scrape it in-cluster through the optional headless metrics Service and restrict it with a NetworkPolicy. Exposing `/metrics` publicly is outside the supported configuration.
:::

```yaml
# Helm values
metrics:
  headlessService: true # creates <release>-metrics, clusterIP None, port 8080
networkPolicy:
  enabled: true
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: monitoring
```

```yaml
# Prometheus scrape config (example)
scrape_configs:
  - job_name: kuvryn-ant
    metrics_path: /metrics
    kubernetes_sd_configs:
      - role: endpoints
    relabel_configs:
      - source_labels: [__meta_kubernetes_service_name]
        regex: ant-ant-metrics
        action: keep
```

Most metrics are process-local counters (they reset when the process restarts). Replication metrics are read from the durable job rows at scrape time and stay monotonic across restarts. Collectors for storage pressure, cache feeds and prewarm only report what this process observed; in practice that is the process holding the writer lease.

## Metric reference

This is the complete list of metrics the server emits. Types are Prometheus types; label values come from closed sets unless noted.

### HTTP and process

| Metric                                  | Type      | Labels                            | Meaning                                                                                        |
| --------------------------------------- | --------- | --------------------------------- | ---------------------------------------------------------------------------------------------- |
| `ant_http_requests_total`               | counter   | `route`, `method`, `status_class` | Completed HTTP requests                                                                        |
| `ant_http_request_duration_seconds`     | histogram | `route`, `method`, `status_class` | Request duration; buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 s            |
| `ant_http_active_requests`              | gauge     | none                              | Requests currently executing                                                                   |
| `ant_http_saturation_total`             | counter   | none                              | Requests rejected by the admission limit (`ANT_MAX_REQUESTS`; answered 503 with `Retry-After`) |
| `ant_database_readiness_failures_total` | counter   | none                              | Failed PostgreSQL readiness checks (`/health/ready`)                                           |
| `ant_data_dir_bytes_free`               | gauge     | none                              | Bytes available on the filesystem holding `ANT_DATA_DIR` (omitted if it cannot be inspected)   |
| `ant_data_dir_bytes_total`              | gauge     | none                              | Capacity of that filesystem                                                                    |

Label values:

- `method`: `GET`, `HEAD`, `POST`, `PATCH`, `PUT`, `DELETE`, `OPTIONS` or `OTHER`.
- `status_class`: `1xx`..`5xx` or `other`.
- `route` is bounded:
  - Management and operational routes keep their registered pattern: `/api/v1/feeds`, `/api/v1/feeds/{id}`, `/api/v1/feeds/{id}/sources`, `/api/v1/feeds/{id}/sources/{source_id}`, `/api/v1/feeds/{id}/publish-policy`, `/api/v1/sources`, `/api/v1/sources/{id}`, `/api/v1/status`, `/health/live`, `/health/ready`, `/metrics`.
  - Package-protocol traffic is `<format>.<class>`, where format is one of `ansible`, `apk`, `apt`, `cargo`, `cocoapods`, `composer`, `conan`, `conda`, `cran`, `git-lfs`, `go`, `helm`, `huggingface`, `maven`, `npm`, `nuget`, `p2`, `pub`, `pypi`, `raw`, `rpm`, `rubygems`, `swift`, `terraform`, and class is `metadata`, `download`, `publish`, `delete` or `other`. Example: `npm.metadata`, `maven.download`, `pypi.publish`.
  - OCI traffic is `oci.<class>` with class `base`, `catalog`, `upload`, `blob`, `manifest`, `tags`, `referrers` or `other`.
  - Federation endpoints are `federation.blob`, `federation.proxy`, `federation.route`, `federation.replication`, `federation.search`, `federation.audit` and `federation.control`.
  - Everything else is `unmatched`.

  The class is derived from the HTTP method and the shape of the path only. Package names, versions, digests and feed slugs never appear in a label.

### Storage and retention

| Metric                                            | Type    | Labels              | Meaning                                                                                                                                |
| ------------------------------------------------- | ------- | ------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| `ant_storage_pressure_enabled`                    | gauge   | none                | 1 when storage-pressure eviction is enabled                                                                                            |
| `ant_storage_usage_ratio`                         | gauge   | none                | Used share of capacity (filesystem or configured quota); 0 when capacity is unknown                                                    |
| `ant_storage_used_bytes`                          | gauge   | none                | Bytes used against the measured capacity                                                                                               |
| `ant_storage_capacity_bytes`                      | gauge   | none                | Capacity in bytes; 0 when unknown                                                                                                      |
| `ant_storage_reclaimable_bytes`                   | gauge   | none                | Unreferenced bytes garbage collection will free                                                                                        |
| `ant_storage_pressure_unevictable`                | gauge   | none                | 1 when usage is above the high watermark and nothing is evictable                                                                      |
| `ant_storage_pressure_last_run_timestamp_seconds` | gauge   | none                | Unix time of the last pressure check                                                                                                   |
| `ant_storage_pressure_evicted_versions_total`     | counter | none                | Versions evicted under storage pressure by this process                                                                                |
| `ant_storage_pressure_evicted_bytes_total`        | counter | none                | Bytes made reclaimable by pressure eviction in this process                                                                            |
| `ant_storage_pressure_state`                      | gauge   | `state`             | One-hot current state: `disabled`, `no_capacity`, `invalid`, `ok`, `pressure`, `unevictable`                                           |
| `ant_storage_evicted_versions_total`              | counter | `purpose`, `reason` | Versions evicted, by feed purpose (`artifacts`, `cache`) and reason (`pressure_lru`; cache feeds also `cache_idle`, `cache_max_bytes`) |
| `ant_storage_evicted_bytes_total`                 | counter | `purpose`, `reason` | Bytes made reclaimable by eviction, same labels                                                                                        |
| `ant_feed_used_bytes`                             | gauge   | `purpose`, `feed`   | Distinct blob bytes referenced by a cache feed at its last measurement (`feed` is the feed slug)                                       |
| `ant_feed_max_bytes`                              | gauge   | `purpose`, `feed`   | Configured size limit of a cache feed, 0 when unlimited                                                                                |

The `ant_storage_pressure_*` gauges other than the eviction counters appear only after the first real (non dry-run) pressure check of the process.

### Quotas and rate limits

| Metric                          | Type    | Labels                               | Meaning                                                                                           |
| ------------------------------- | ------- | ------------------------------------ | ------------------------------------------------------------------------------------------------- |
| `ant_ratelimit_throttled_total` | counter | `scope`                              | Requests rejected with 429 by a rate limit; `scope` is `principal`, `feed`, `namespace` or `site` |
| `ant_quota_rejections_total`    | counter | `scope`                              | Publications rejected by a storage quota; `scope` is `feed` or `namespace`                        |
| `ant_quota_usage`               | gauge   | `scope`, `feed`, `namespace`, `unit` | Last observed quota usage; `unit` is `bytes` or `packages`; `feed` is the feed ID                 |
| `ant_quota_limit`               | gauge   | `scope`, `feed`, `namespace`, `unit` | Configured quota limit                                                                            |

Quota series are bounded by the configured rules.

### Prewarm

| Metric                    | Type    | Labels   | Meaning                                                                                              |
| ------------------------- | ------- | -------- | ---------------------------------------------------------------------------------------------------- |
| `ant_prewarm_items_total` | counter | `result` | Items finished: `warmed` (fetched from an upstream), `cached` (already present), `failed`, `skipped` |
| `ant_prewarm_bytes_total` | counter | none     | Bytes read from feeds while prewarming                                                               |
| `ant_prewarm_runs_total`  | counter | `state`  | Runs finished: `succeeded`, `partial`, `failed`, `canceled`                                          |
| `ant_prewarm_active_runs` | gauge   | none     | Runs executing in this process                                                                       |

Per-job and per-item detail, including failure codes, is in the API and the console, not in labels. See [Prewarm](./prewarm.md).

### Replication

All per-job series carry the label `job` with the operator-chosen job name. No peer URLs, tokens or feed identifiers are emitted.

| Metric                                       | Type    | Labels | Meaning                                                                                          |
| -------------------------------------------- | ------- | ------ | ------------------------------------------------------------------------------------------------ |
| `ant_replication_transfer_bytes_total`       | counter | none   | Blob bytes received from replication peers by this process, including failed or resumed attempts |
| `ant_replication_scrape_errors_total`        | counter | none   | Emitted (value 1) only when job state could not be read for this scrape                          |
| `ant_replication_lag_seconds`                | gauge   | `job`  | Age of the oldest change not yet applied (0 when caught up)                                      |
| `ant_replication_changes_behind`             | gauge   | `job`  | Peer changes not yet applied                                                                     |
| `ant_replication_seconds_since_last_success` | gauge   | `job`  | Seconds since the last successful run (-1 if never)                                              |
| `ant_replication_running`                    | gauge   | `job`  | 1 while a worker holds the job                                                                   |
| `ant_replication_consecutive_failures`       | gauge   | `job`  | Failed runs since the last success                                                               |
| `ant_replication_failures_total`             | counter | `job`  | Failed runs                                                                                      |
| `ant_replication_bytes_transferred_total`    | counter | `job`  | Blob bytes received for the job                                                                  |
| `ant_replication_blobs_transferred_total`    | counter | `job`  | Blobs transferred for the job                                                                    |
| `ant_replication_blobs_deduplicated_total`   | counter | `job`  | Blobs skipped because they were already present                                                  |
| `ant_replication_resumed_bytes_total`        | counter | `job`  | Staged bytes that resumed transfers did not fetch again                                          |
| `ant_replication_reseeds_total`              | counter | `job`  | Automatic reseeds after the peer purged needed history                                           |
| `ant_replication_backfills_pending`          | gauge   | `job`  | Attached sources queued for back-fill                                                            |
| `ant_replication_conflicts_total`            | counter | `job`  | Items left untouched because destination content took precedence                                 |

### Federation

Emitted only when federation is configured (see [Federation](../replication/federation.md)). `ant_federation_active` is always present in that case; the rest appear once a membership document has been applied.

| Metric                                         | Type    | Labels                 | Meaning                                                                             |
| ---------------------------------------------- | ------- | ---------------------- | ----------------------------------------------------------------------------------- |
| `ant_federation_active`                        | gauge   | none                   | 1 when this instance is an active federation member                                 |
| `ant_federation_document_generation`           | gauge   | `kind`                 | Applied document generation; `kind` is `membership`, `policy`, `topology`           |
| `ant_federation_authority_epoch`               | gauge   | none                   | Applied authority epoch                                                             |
| `ant_federation_authority`                     | gauge   | none                   | 1 when this instance holds the authority role                                       |
| `ant_federation_members`                       | gauge   | `status`               | Members by status: `active`, `suspended`, `removed`                                 |
| `ant_federation_member_up`                     | gauge   | `member`               | 1 if the last request to the member succeeded; `member` is the member name          |
| `ant_federation_member_rtt_seconds`            | gauge   | `member`               | Smoothed round-trip time to the member over the federation listener                 |
| `ant_federation_document_age_seconds`          | gauge   | `kind`                 | Time since the applied policy document was last confirmed current (`kind="policy"`) |
| `ant_federation_policy_unresolved_feed_keys`   | gauge   | none                   | Policy feed keys not bound to a local feed (inert grants)                           |
| `ant_federation_topology_rules_targeting_self` | gauge   | none                   | Replication rules that target this member                                           |
| `ant_federation_topology_home_feeds`           | gauge   | none                   | Federated feeds this member is home of                                              |
| `ant_federation_transfers_active`              | gauge   | none                   | Home transfers in progress                                                          |
| `ant_federation_quarantines_unresolved`        | gauge   | none                   | Quarantined old-home sources awaiting review                                        |
| `ant_federation_alert`                         | gauge   | `code`, `severity`     | Active federation alerts (value 1 per code and severity)                            |
| `ant_federation_assertions_total`              | counter | `outcome`              | Principal assertions: `issued`, `accepted`, `rejected`                              |
| `ant_federation_join_rejected_total`           | counter | none                   | Refused join attempts                                                               |
| `ant_federation_search_requests_total`         | counter | none                   | Federated searches fanned out by this member                                        |
| `ant_federation_search_partial_total`          | counter | none                   | Federated searches that reported a member as not ok                                 |
| `ant_federation_search_cache_hits_total`       | counter | none                   | Per-member search answers served from the 30 s cache                                |
| `ant_federation_search_rate_limited_total`     | counter | none                   | Peer search requests refused by the per-peer or per-principal limit                 |
| `ant_federation_route_requests_total`          | counter | `mechanism`, `outcome` | Routed reads by mechanism (`proxy`, `redirect`, `blob_url`) and outcome             |
| `ant_federation_proxy_bytes_total`             | counter | none                   | Bytes streamed to clients from other members by the federation proxy                |
| `ant_federation_proxy_active`                  | gauge   | none                   | Proxied reads in progress                                                           |
| `ant_federation_route_served_total`            | counter | none                   | Proxied reads and blob URLs this member served for other members                    |
| `ant_federation_route_served_bytes_total`      | counter | none                   | Bytes served for other members' proxied reads and blob URLs                         |
| `ant_federation_route_writes_not_routed_total` | counter | none                   | Writes to federated feeds answered locally (writes are never routed)                |
| `ant_federation_route_cache_total`             | counter | `event`                | Entry-cache events of proxied immutable reads: `hit`, `store`, `drop`               |
| `ant_federation_blob_urls_total`               | counter | `event`                | Signed blob URLs: `issued`, `accepted`, `refused`                                   |
| `ant_federation_route_circuits_open`           | gauge   | none                   | Members currently excluded from routing by an open circuit                          |

### Suggested alerts

| Condition                            | Example expression                                                                                           |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------------ |
| Sustained 5xx ratio                  | `sum(rate(ant_http_requests_total{status_class="5xx"}[5m])) / sum(rate(ant_http_requests_total[5m])) > 0.02` |
| Admission limit shedding load        | `increase(ant_http_saturation_total[10m]) > 0`                                                               |
| Database readiness failing           | `increase(ant_database_readiness_failures_total[5m]) > 0`                                                    |
| Slow route                           | `histogram_quantile(0.95, sum by (le, route) (rate(ant_http_request_duration_seconds_bucket[5m]))) > 2`      |
| Data volume filling (warning / page) | `ant_data_dir_bytes_free / ant_data_dir_bytes_total < 0.2` for 10m / `< 0.1`                                 |
| Storage full and nothing evictable   | `ant_storage_pressure_unevictable == 1`                                                                      |
| Replication stalled                  | `ant_replication_consecutive_failures > 3` or `ant_replication_lag_seconds > 3600`                           |
| Federation member unreachable        | `ant_federation_member_up == 0` for 5m                                                                       |

## Health and operational APIs

| Endpoint                                                       | Auth     | Meaning                                                                                                                        |
| -------------------------------------------------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `GET /health/live`                                             | none     | Process is alive; always answered, even when the admission limit is saturated                                                  |
| `GET /health/ready`                                            | none     | PostgreSQL reachable within 2 s and the process is not stopping; 503 otherwise                                                 |
| `GET /api/v1/status`                                           | optional | Name and version anonymously; with an instance-admin token also database, blob storage and writer-lease state, plus `warnings` |
| `GET /api/v1/metrics/traffic`                                  | admin    | Per-feed and per-source request and byte time series                                                                           |
| `GET /api/v1/source-health`, `GET /api/v1/sources/{id}/health` | admin    | Upstream health of remote sources                                                                                              |
| `GET /api/v1/stats/feeds`, `GET /api/v1/stats/sources`         | admin    | Usage statistics                                                                                                               |

The console health page shows the same data. See [Diagnostics and repair](./diagnostics.md) for details.

## Logs

Logs are JSON lines on stderr (`log/slog`). Every request produces one `HTTP request completed` record with `request_id`, `route`, `method`, `status` and `duration_seconds`; successful health and metrics scrapes are not logged. With tracing enabled the record also carries `trace_id`, which equals the trace ID in your tracing backend. The `request_id` is returned to clients in the `X-Request-ID` header and in every management API error body.

The process keeps the most recent 500 WARN and ERROR records in memory; they are included in the diagnostics bundle downloaded from the API.

Background workers log their outcome, for example `retention run` (versions deleted, blobs marked and swept, integrity checks), `storage pressure` (state, evictions) and `audit purge holding back events the audit sink has not exported`.

## Tracing

Tracing is off by default. Enable it with environment variables or the chart's `observability.tracing` values:

| Variable                | Chart value                         | Meaning                                                                                                                                        |
| ----------------------- | ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------- |
| `ANT_OTEL_ENABLED`      | `observability.tracing.enabled`     | `true` turns tracing on; nothing is exported otherwise                                                                                         |
| `ANT_OTEL_ENDPOINT`     | `observability.tracing.endpoint`    | Optional OTLP base URL, for example `http://otel-collector:4318`. Must be http(s) without credentials. Overrides `OTEL_EXPORTER_OTLP_ENDPOINT` |
| `ANT_OTEL_PROTOCOL`     | `observability.tracing.protocol`    | `http/protobuf` (default) or `grpc`. Falls back to `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`, then `OTEL_EXPORTER_OTLP_PROTOCOL`                    |
| `ANT_OTEL_SERVICE_NAME` | `observability.tracing.serviceName` | Defaults to `kuvryn-ant` (or `OTEL_SERVICE_NAME`)                                                                                              |
| `ANT_OTEL_SAMPLE_RATIO` | `observability.tracing.sampleRatio` | Parent-based head sampling ratio, greater than 0 and at most 1; default 1                                                                      |

```yaml
observability:
  tracing:
    enabled: true
    endpoint: http://otel-collector.observability:4318
    protocol: http/protobuf
    serviceName: kuvryn-ant
    sampleRatio: 0.2
```

The exporters also honour the standard `OTEL_EXPORTER_OTLP_*` variables (headers, timeout, compression, TLS). Put authentication headers in a Kubernetes Secret and inject them through a platform mechanism: the chart's `extraEnv` accepts only `ANT_`-prefixed plain values by design.

Incoming W3C `traceparent` headers are honoured.

| Span                                                                                | Kind     | Notes                                                                                                                                                                                        |
| ----------------------------------------------------------------------------------- | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `METHOD <route pattern>`                                                            | server   | Management API and every package protocol, for example `GET /api/v1/feeds/{id}`. Attributes `http.route`, `http.request.method`, `http.response.status_code`. Liveness probes are not traced |
| `db select`, `db insert`, `db begin`, `db commit`, ...                              | client   | One per SQL statement inside a traced request; only the SQL verb, never SQL text or arguments                                                                                                |
| `blob stat`, `blob open`, `blob stage`, `blob finalize`, `blob delete`, `blob list` | internal | Blob store operations (disk and S3)                                                                                                                                                          |
| `upstream fetch`                                                                    | internal | Remote-source fetches, with `server.address` and status code                                                                                                                                 |

Errors set the span status to `error` without the error text, because driver and backend errors can contain sensitive detail.

::: warning Gap
Background workers (retention, replication, audit forwarding) are not traced.
:::

## Audit log

Every management mutation writes an audit event in the same database transaction as the change itself, so a committed change always has its audit record and a rolled-back change never does. Authentication denials, break-glass sign-ins, retention deletions, pressure evictions and diagnostics downloads are audited too.

- Query: `GET /api/v1/audit` (console: Audit).
- Export: `GET /api/v1/audit/export`.

### Audit retention

A worker running under the instance writer lease purges old `audit_events`:

| Variable                   | Chart value                    | Default            | Range                                            |
| -------------------------- | ------------------------------ | ------------------ | ------------------------------------------------ |
| `ANT_AUDIT_RETENTION`      | `auditRetention.retain`        | `9600h` (400 days) | `0` keeps everything, otherwise `24h`..`175200h` |
| `ANT_AUDIT_PURGE_INTERVAL` | `auditRetention.purgeInterval` | `1h`               | `1m`..`720h`                                     |
| `ANT_AUDIT_PURGE_BATCH`    | `extraEnv`                     | `5000`             | 1..100000                                        |

If an audit sink is configured, only events at or below its delivery checkpoint are eligible for deletion. A lagging, failing or even disabled sink holds deletion back, and the worker logs `audit purge holding back events the audit sink has not exported`. Delete the sink configuration to release the hold. Export to your SIEM if you need longer retention than the database should carry.

### Audit sink (SIEM streaming)

The audit sink forwards audit events to one operator-configured destination, in order and at least once, with a durable checkpoint (the highest delivered event ID).

| Kind     | Endpoint                                                     | Format                                        |
| -------- | ------------------------------------------------------------ | --------------------------------------------- |
| `syslog` | `udp://host:port`, `tcp://host:port` or `tls://host:port`    | RFC 5424 syslog                               |
| `http`   | `https://...` (no credentials, query or fragment in the URL) | NDJSON over HTTPS; optional bearer credential |

Loopback, link-local, multicast and unspecified targets are refused, both at configuration time and again at dial time after DNS resolution. Private ranges are allowed, because SIEM collectors commonly live inside the network. The credential is stored encrypted with the root keyring and is never returned or logged.

Configure it in the console or through `GET`/`PUT`/`DELETE /api/v1/audit-sink` (instance admin). To create it the first time, read the sink (it reports `configured: false` and `revision: 1`) and send `If-Match: "1"`:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
curl -fsS $ANT/api/v1/audit-sink -H "$H"
curl -fsS -X PUT $ANT/api/v1/audit-sink -H "$H" -H "$J" -H 'If-Match: "1"' \
  -H 'Idempotency-Key: audit-sink-1' \
  -d '{"kind":"http","endpoint":"https://siem.example.com/ingest","enabled":true,"credential":"<SIEM_TOKEN>"}'
```

Omit `credential` on later updates to keep the stored one, or send an empty string to remove it. The GET response shows `checkpoint`, `last_forwarded_at` and `last_error`.
