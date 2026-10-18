import { useEffect, useMemo, useState } from "react";
import type { Client } from "./api";
import type { Go } from "./App";
import { useAllFeeds, useAllSources, useOpsMaps } from "./data";
import { useOptionalCall } from "./gaps";
import {
  Alert,
  Badge,
  Button,
  ChartLegend,
  LineChart,
  Meter,
  SegmentedControl,
  StatCard,
  Table,
} from "./ds/bundle";
import type {
  AuditEvent,
  ComponentStatus,
  WriterLeaseStatus,
} from "./generated";
import type { PackageRef } from "./Packages";
import {
  Card,
  Failure,
  Loading,
  Mono,
  PageHeading,
  SectionHead,
  Unavailable,
  bytes,
  count,
  useResource,
  when,
} from "./ui";

const HOUR = 3600000;
const ranges = {
  "1h": { span: HOUR, step: "5m" },
  "24h": { span: 24 * HOUR, step: "1h" },
  "7d": { span: 7 * 24 * HOUR, step: "1d" },
} as const;

function ComponentBadge({ c }: { c: ComponentStatus }) {
  const tone =
    c.status === "ok" ? "good" : c.status === "degraded" ? "warn" : "bad";
  return (
    <Badge tone={c.status === "unknown" ? "neutral" : tone} dot>
      {c.status === "ok" ? "Ready" : c.status}
      {c.latency_ms != null && ` · ${c.latency_ms} ms`}
    </Badge>
  );
}
function LeaseBadge({ l }: { l: WriterLeaseStatus }) {
  return (
    <Badge tone={l.state === "held" ? "good" : "warn"} dot>
      {l.state.replace("_", " ")}
    </Badge>
  );
}

export function Overview({
  client,
  go,
  openPackage,
}: {
  client: Client;
  go: Go;
  openPackage: (p: PackageRef) => void;
}) {
  const status = useResource(() => client.call("getStatus"), [client]);
  const [checked, setChecked] = useState("");
  useEffect(() => {
    if (status.value) setChecked(new Date().toLocaleTimeString());
  }, [status.value]);
  const feeds = useAllFeeds(client);
  const sources = useAllSources(client);
  const ops = useOpsMaps(client);
  const [range, setRange] = useState<keyof typeof ranges>("24h");
  const window = useMemo(() => {
    const to = new Date();
    return {
      from: new Date(to.getTime() - ranges[range].span).toISOString(),
      to: to.toISOString(),
    };
  }, [range]);
  const traffic = useOptionalCall(client, "getTrafficMetrics", {
    query: {
      scope: "instance",
      step: ranges[range].step,
      from: window.from,
      to: window.to,
    },
  });
  const storage = useOptionalCall(client, "getStorageUsage");
  const recent = useOptionalCall(client, "searchPackages", {
    query: { limit: "4", origin: "local" },
  });
  const audit = useOptionalCall(client, "listAuditEvents", {
    query: { limit: "4" },
  });
  const readiness = status.value?.data.readiness;
  const failing = sources.items.filter(
    (s) => ops.health.get(s.id)?.status === "failing",
  );
  const reload = () => {
    status.reload();
    feeds.reload();
    sources.reload();
    ops.reload();
    traffic.reload();
    storage.reload();
    recent.reload();
    audit.reload();
  };
  const t = traffic.state === "ready" ? traffic.data : undefined;
  const st = storage.state === "ready" ? storage.data : undefined;
  const packages = [...ops.feeds.values()].reduce((n, f) => n + f.packages, 0);
  const hit = t?.totals.cache_hit_ratio;
  return (
    <>
      <PageHeading
        eyebrow="Instance"
        title="Overview"
        text="Health of the artifact service, storage and upstream sources."
      >
        <Button variant="secondary" icon="refresh-cw" onClick={reload}>
          Refresh
        </Button>
        <Button icon="plus" onClick={() => go("feeds", { create: true })}>
          Create feed
        </Button>
      </PageHeading>
      {failing.map((s) => {
        const h = ops.health.get(s.id)!;
        return (
          <Alert
            key={s.id}
            tone="bad"
            title={`${s.name} cannot reach its upstream`}
            className="spaced"
            action={
              <Button
                size="sm"
                variant="secondary"
                onClick={() => go("sources", { source: s.id })}
              >
                Open source
              </Button>
            }
          >
            {h.last_status_code ? `HTTP ${h.last_status_code}. ` : ""}
            {h.last_error_at && `Since ${when(h.last_error_at)}. `}
            Cached content is still served.
          </Alert>
        );
      })}
      <div className="grid-auto grid-auto--210">
        <Card
          title="Service"
          actions={
            <Badge
              tone={
                status.error
                  ? "bad"
                  : readiness?.ready === false
                    ? "warn"
                    : "good"
              }
              dot
            >
              {status.error
                ? "Unreachable"
                : readiness?.ready === false
                  ? "Degraded"
                  : "Reachable"}
            </Badge>
          }
        >
          <Failure error={status.error} />
          {status.loading ? (
            <Loading what="service status" />
          ) : (
            status.value && (
              <dl className="kv">
                <dt>Service</dt>
                <dd>{status.value.data.name}</dd>
                <dt>API version</dt>
                <dd>{status.value.data.api_version}</dd>
                <dt>Last successful check</dt>
                <dd>{checked}</dd>
                {readiness && (
                  <>
                    <dt>PostgreSQL</dt>
                    <dd>
                      <ComponentBadge c={readiness.database} />
                    </dd>
                    <dt>Blob storage</dt>
                    <dd>
                      <ComponentBadge c={readiness.blob_storage} />
                    </dd>
                    <dt>Writer lease</dt>
                    <dd>
                      <LeaseBadge l={readiness.writer_lease} />
                    </dd>
                  </>
                )}
              </dl>
            )
          )}
          {status.value && !readiness && (
            <p className="muted small">
              Reachability only. Readiness detail is shown to instance admins.
            </p>
          )}
        </Card>
        <StatCard
          label="Packages"
          value={ops.feeds.size ? count(packages) : "—"}
          icon="package"
          sub={`across ${feeds.items.length} feeds · ${sources.items.length} sources`}
        />
        <StatCard
          label="Requests"
          value={t ? count(t.totals.pulls + t.totals.publishes) : "—"}
          unit={t ? `/ ${range}` : undefined}
          icon="arrow-down-up"
          sub={t ? "pulls + publishes" : "Metrics not available"}
        />
        <StatCard
          label="Cache hit ratio"
          value={hit != null ? (hit * 100).toFixed(1) : "—"}
          unit={hit != null ? "%" : undefined}
          icon="zap"
          sub={t ? `remote sources, ${range}` : "Metrics not available"}
        />
      </div>
      <Failure error={feeds.error ?? sources.error} />
      <div className="grid-auto grid-auto--380 spaced">
        <Card
          title="Traffic"
          actions={
            <SegmentedControl
              mono
              aria-label="Traffic range"
              options={Object.keys(ranges)}
              value={range}
              onChange={(v: string) => setRange(v as keyof typeof ranges)}
            />
          }
        >
          {traffic.state === "loading" && <Loading what="traffic" />}
          {traffic.state === "unavailable" && (
            <Unavailable what="Traffic metrics" />
          )}
          {traffic.state === "error" && <Failure error={traffic.error} />}
          {t && (
            <>
              <LineChart
                height={170}
                series={[
                  { data: t.points.map((p) => p.pulls) },
                  { data: t.points.map((p) => p.publishes) },
                ]}
                labels={t.points
                  .filter(
                    (_, i) => i % Math.ceil(t.points.length / 5 || 1) === 0,
                  )
                  .map((p) =>
                    new Date(p.bucket).toLocaleString([], {
                      hour: "2-digit",
                      minute: "2-digit",
                      month: range === "7d" ? "short" : undefined,
                      day: range === "7d" ? "numeric" : undefined,
                    }),
                  )}
              />
              <ChartLegend
                items={[
                  { label: "Pulls", value: count(t.totals.pulls) },
                  { label: "Publishes", value: count(t.totals.publishes) },
                  { label: "Errors", value: count(t.totals.errors) },
                ]}
              />
            </>
          )}
        </Card>
        <Card
          title="Storage"
          actions={st ? <Mono muted>{st.backend}</Mono> : undefined}
        >
          {storage.state === "loading" && <Loading what="storage usage" />}
          {storage.state === "unavailable" && (
            <Unavailable what="Storage usage" />
          )}
          {storage.state === "error" && <Failure error={storage.error} />}
          {st && (
            <>
              <Meter
                label="Stored vs logical size"
                valueLabel={`${bytes(st.blob_bytes)} / ${bytes(st.logical_bytes)}`}
                value={
                  st.logical_bytes
                    ? Math.round((st.blob_bytes / st.logical_bytes) * 100)
                    : 0
                }
              />
              {st.staging_bytes != null && (
                <p className="muted small">
                  Upload staging: {bytes(st.staging_bytes)}
                </p>
              )}
              <p className="muted small">
                {count(st.blob_count)} blobs · deduplication saved{" "}
                {bytes(st.dedup_saved_bytes)} · {bytes(st.unreferenced_bytes)}{" "}
                unreferenced ({count(st.gc_candidates)} collectable) · measured{" "}
                {when(st.measured_at)}
              </p>
            </>
          )}
        </Card>
      </div>
      <div className="grid-auto grid-auto--380">
        <div>
          <SectionHead title="Recently published">
            <Button
              size="sm"
              variant="ghost"
              iconRight="arrow-right"
              onClick={() => go("packages")}
            >
              All packages
            </Button>
          </SectionHead>
          {recent.state === "ready" ? (
            <Table
              onRowClick={(r: PackageRef) => openPackage(r)}
              columns={[
                {
                  key: "coordinate",
                  label: "Package",
                  render: (r: PackageRef) => (
                    <>
                      {r.coordinate}
                      <small>{r.feed_slug}</small>
                    </>
                  ),
                },
                { key: "format", label: "Format" },
              ]}
              rows={recent.data.items.slice(0, 4)}
            />
          ) : recent.state === "loading" ? (
            <Loading what="recent packages" />
          ) : recent.state === "error" ? (
            <Failure error={recent.error} />
          ) : (
            <Unavailable what="Cross-feed package search" />
          )}
        </div>
        <div>
          <SectionHead title="Recent activity">
            <Button
              size="sm"
              variant="ghost"
              iconRight="arrow-right"
              onClick={() => go("audit")}
            >
              Audit log
            </Button>
          </SectionHead>
          {audit.state === "ready" ? (
            <Table
              columns={[
                {
                  key: "action",
                  label: "Event",
                  render: (r: AuditEvent) => (
                    <>
                      <Mono>{r.action}</Mono>
                      <small>{r.principal_id ?? "anonymous"}</small>
                    </>
                  ),
                },
                {
                  key: "result",
                  label: "Result",
                  render: (r: AuditEvent) => (
                    <Badge
                      tone={
                        r.result === "success"
                          ? "good"
                          : r.result === "denied"
                            ? "bad"
                            : "warn"
                      }
                      dot
                    >
                      {r.result}
                    </Badge>
                  ),
                },
                {
                  key: "created_at",
                  label: "Time",
                  align: "right",
                  render: (r: AuditEvent) => when(r.created_at),
                },
              ]}
              rows={audit.data.items.slice(0, 4)}
            />
          ) : audit.state === "loading" ? (
            <Loading what="recent activity" />
          ) : audit.state === "error" ? (
            <Failure error={audit.error} />
          ) : (
            <Unavailable what="The audit log" />
          )}
        </div>
      </div>
    </>
  );
}
