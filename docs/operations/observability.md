# Observability

Kuvryn Ant exposes Prometheus metrics, structured JSON logs, optional
OpenTelemetry traces, and a redacted diagnostics bundle (spec section 6.13).
None of them record request paths, query strings, credentials, principals or
resource IDs.

## Metrics

`GET /metrics` (Prometheus text format) is served on the application port. It
is not routed by the chart ingress; scrape it through the optional headless
metrics Service and restrict it with NetworkPolicy.

| Metric | Type | Labels |
| --- | --- | --- |
| `ant_http_requests_total` | counter | `route`, `method`, `status_class` |
| `ant_http_request_duration_seconds` | histogram | `route`, `method`, `status_class` |
| `ant_http_active_requests` | gauge | none |
| `ant_http_saturation_total` | counter | none (requests shed by `ANT_MAX_REQUESTS`) |
| `ant_database_readiness_failures_total` | counter | none |
| `ant_data_dir_bytes_free` | gauge | none (bytes available on the filesystem holding `ANT_DATA_DIR`) |
| `ant_data_dir_bytes_total` | gauge | none (capacity of that filesystem) |

`route` is a registered route pattern or `unmatched`, so cardinality is bounded.
Suggested alerts: sustained 5xx ratio, `ant_http_saturation_total` increasing,
`ant_database_readiness_failures_total` increasing, and p95 duration by route.
For the data volume alert on low free space, for example
`ant_data_dir_bytes_free / ant_data_dir_bytes_total < 0.2` for 10 minutes
(warning) and `< 0.1` (page).

Health endpoints: `GET /health/live` (process), `GET /health/ready`
(PostgreSQL reachable, not stopping). Administrators get dependency detail
(database, blob storage, writer lease) from `GET /api/v1/status` with a bearer
token, and the console health page shows the same data. Per-feed/source traffic
and upstream health are available through `/api/v1/metrics/traffic` and
`/api/v1/source-health`.

## Logs

Logs are JSON on stderr (`log/slog`). Every request produces one
`HTTP request completed` line with `request_id`, `route`, `method`, `status`
and `duration_seconds`; successful health and metrics scrapes are silent.
When tracing is enabled the line also carries `trace_id`, which equals the
trace ID in your tracing backend, so a log line leads directly to its trace.
The `request_id` is also returned to clients in `X-Request-ID` and error bodies.

The process keeps the most recent 500 WARN and ERROR records in memory; they
appear in the diagnostics bundle.

## Tracing

Tracing is off by default. Enable it with environment variables (chart values
under `observability.tracing`):

| Variable | Meaning |
| --- | --- |
| `ANT_OTEL_ENABLED` | `true` turns tracing on. Default off; nothing is exported otherwise. |
| `ANT_OTEL_ENDPOINT` | Optional OTLP base URL, for example `http://otel-collector:4318`. Overrides `OTEL_EXPORTER_OTLP_ENDPOINT`. Must not contain credentials. |
| `ANT_OTEL_PROTOCOL` | `http/protobuf` (default) or `grpc`. Falls back to `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` / `OTEL_EXPORTER_OTLP_PROTOCOL`. |
| `ANT_OTEL_SERVICE_NAME` | Defaults to `kuvryn-ant` (or `OTEL_SERVICE_NAME`). |
| `ANT_OTEL_SAMPLE_RATIO` | Parent-based head sampling ratio, greater than 0 and at most 1. Default 1. |

The exporters also honor the standard `OTEL_EXPORTER_OTLP_*` variables
(headers, timeout, compression, TLS certificates). Put authentication headers
in a Kubernetes Secret and inject them with a platform mechanism; the chart's
`extraEnv` only accepts `ANT_`-prefixed plain values by design.

Spans (W3C `traceparent` is honored on incoming requests):

| Span name | Kind | Notes |
| --- | --- | --- |
| `METHOD <route pattern>` | server | management API and every package protocol; for example `GET /api/v1/feeds/{id}`. Attributes: `http.route`, `http.request.method`, `http.response.status_code`. Liveness probes are not traced. |
| `db select`, `db insert`, `db begin`, `db commit`, ... | client | one per SQL statement inside a traced request; transactions show as `db begin` ... `db commit`. Only the SQL verb is recorded, never SQL text or arguments. |
| `blob stat`, `blob open`, `blob stage`, `blob finalize`, `blob delete`, `blob list` | internal | blob store operations (disk and S3). |
| `upstream fetch` | internal | remote-source fetches, with `server.address` and status code. |

Errors set span status to `error` without error text, because driver and backend
errors can contain sensitive detail. Background workers (retention, replication,
audit forwarding) are not traced.

## Diagnostics bundle

For support, produce a redacted `tar.gz`:

```sh
ant diagnostics                 # writes ant-diagnostics-<timestamp>.tar.gz (mode 0600)
ant diagnostics -o bundle.tgz   # explicit path; -o - writes to stdout
curl -H "Authorization: Bearer $ADMIN_TOKEN" -o bundle.tgz https://ant.example/api/v1/diagnostics
```

The CLI uses the same environment as `ant serve`. It never migrates, and it
still produces a bundle when PostgreSQL or storage is unreachable (those become
failed checks). The API endpoint requires instance admin, is audited
(`diagnostics.download`) and additionally includes the running service's
readiness detail and its in-memory recent errors; the CLI's error log only
covers its own short-lived process, so prefer the API (or `kubectl logs`) for
incident evidence.

| File | Content |
| --- | --- |
| `build.json` | version, VCS revision, Go version, OS/arch |
| `config.json` | effective configuration (allowlisted fields only) |
| `health.json` | readiness detail: database, blob storage, writer lease |
| `migrations.json` | applied schema version vs the binary's version, pending count |
| `checks.json` | database ping and PostgreSQL version; blob backend reachability, with timings |
| `errors.log` | most recent WARN/ERROR log records (JSON lines) |
| `runtime.json` | goroutines, heap, GC and CPU counts |
| `goroutines.txt` | full goroutine dump |

Redaction is layered. The configuration is built from an allowlist: the database
password, root key and storage credentials are replaced by `[REDACTED]`, and
credential file paths are shown without contents. Every file is then scrubbed:
literal secret values (database password, root key in raw, hex and base64 forms),
`postgres://user:password@` URLs, `Bearer`/`Basic` credentials, `ant_` API
tokens and `password|secret|token|api_key|authorization|credential` key/value
pairs. A unit test seeds each of these and fails if any survives in the archive.
Redaction is best-effort defense in depth: review a bundle before sharing it
outside your organization, and never paste `ANT_*` secret material into
free-text fields that end up in logs.

## Audit retention

`audit_events` is purged by a worker running under the instance writer lease
(`ANT_AUDIT_RETENTION`, default `9600h` = 400 days, `0` keeps everything;
`ANT_AUDIT_PURGE_INTERVAL`, `ANT_AUDIT_PURGE_BATCH`). If an audit sink is
configured, only events at or below its delivery checkpoint are eligible: a
lagging, failing or even disabled sink holds deletion back, and the worker logs
`audit purge holding back events the audit sink has not exported`. Delete the
sink configuration to release the hold. Export to your SIEM if you need longer
retention than the database should carry.

## Metadata repair

`ant repair metadata --dry-run` reports orphaned or inconsistent metadata rows
(dangling tags, subject links, assets without blob or version rows, versions
without component or assets, components without versions, stale integrity
findings) without changing anything; run it without `--dry-run` to fix them in
one transaction. It prints a JSON report of every check, including the count
found and fixed. It refuses to run if the schema does not match the binary,
and never deletes blob objects (garbage collection owns those). Assets whose
blob object is missing from storage are reported but not changed: restore the
object from backup or a replica. A dry run reports the current state, so a
cascade (for example a component that becomes empty only after its last
assetless version is removed) can show up only in the apply report. Take a
backup first and run during low traffic.
