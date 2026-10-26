import { useEffect, useMemo, useState } from "react";
import type { Client } from "./api";
import type { Go } from "./App";
import { useAllFeeds, useAllSources, useNames, useOpsMaps } from "./data";
import { useOptionalCall } from "./gaps";
import {
  Badge,
  Button,
  ChartLegend,
  Icon,
  LineChart,
  Meter,
  Modal,
  SegmentedControl,
  Table,
  Tabs,
} from "./ds/bundle";
import type {
  AuditEvent,
  SourceCacheStats,
  ComponentStatus,
  Feed,
  ReplicationStatusEntry,
  Source,
  SourceHealth,
  WriterLeaseStatus,
} from "./generated";
import type { PackageRef } from "./Packages";
import {
  Card,
  Failure,
  Loading,
  Mono,
  PageHeading,
  Unavailable,
  bytes,
  count,
  useResource,
  shortId,
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

const CAP = 5;

function Kpi({
  label,
  value,
  unit,
  sub,
  icon,
  onClick,
}: {
  label: string;
  value: string;
  unit?: string;
  sub: string;
  icon: string;
  onClick?: () => void;
}) {
  if (onClick)
    return (
      <button
        type="button"
        className="az-card ov-kpi ov-kpi--action"
        onClick={onClick}
      >
        <KpiBody label={label} value={value} unit={unit} sub={sub} icon={icon} />
      </button>
    );
  return (
    <div className="az-card ov-kpi">
      <KpiBody label={label} value={value} unit={unit} sub={sub} icon={icon} />
    </div>
  );
}

function KpiBody({
  label,
  value,
  unit,
  sub,
  icon,
}: {
  label: string;
  value: string;
  unit?: string;
  sub: string;
  icon: string;
}) {
  return (
    <>
      <div className="ov-kpi__label">
        <Icon name={icon} size={14} />
        {label}
      </div>
      <div className="ov-kpi__value">
        {value}
        {unit && <span>{unit}</span>}
      </div>
      <div className="ov-kpi__sub" title={sub}>
        {sub}
      </div>
    </>
  );
}

const pct = (r?: number) => (r == null ? "—" : `${(r * 100).toFixed(1)}%`);

function CacheBreakdown({
  client,
  go,
  range,
  from,
  to,
  onClose,
}: {
  client: Client;
  go: Go;
  range: string;
  from: string;
  to: string;
  onClose: () => void;
}) {
  const stats = useOptionalCall(client, "getCacheStats", {
    query: { from, to },
  });
  return (
    <Modal
      title="Cache hit ratio per remote"
      description={`Last ${range}. Served from Ant or revalidated with a 304, versus downloaded from upstream. Not-found answers and upstream errors are not counted.`}
      onClose={onClose}
      width={760}
      actions={
        <Button variant="secondary" onClick={onClose}>
          Close
        </Button>
      }
    >
      {stats.state === "loading" && <Loading what="cache statistics" />}
      {stats.state === "unavailable" && (
        <Unavailable what="Cache statistics" />
      )}
      {stats.state === "error" && <Failure error={stats.error} />}
      {stats.state === "ready" &&
        (stats.data.items.length === 0 ? (
          <p className="muted">No remote sources.</p>
        ) : (
          <Table
            onRowClick={(r: SourceCacheStats) => {
              onClose();
              go("sources", { source: r.source_id });
            }}
            columns={[
              { key: "name", label: "Source" },
              { key: "format", label: "Format" },
              {
                key: "ratio",
                label: "Hit ratio",
                render: (r: SourceCacheStats) => (
                  <span className="ov-ratio">
                    <span className="ov-ratio__bar">
                      <span
                        style={{
                          width: `${Math.round((r.cache_hit_ratio ?? 0) * 100)}%`,
                        }}
                      />
                    </span>
                    {pct(r.cache_hit_ratio)}
                  </span>
                ),
              },
              {
                key: "hits",
                label: "Served",
                render: (r: SourceCacheStats) => count(r.cache_hits),
              },
              {
                key: "reval",
                label: "Revalidated",
                render: (r: SourceCacheStats) => count(r.cache_revalidations),
              },
              {
                key: "misses",
                label: "Downloaded",
                render: (r: SourceCacheStats) => count(r.cache_misses),
              },
            ]}
            rows={stats.data.items}
            rowKey="source_id"
          />
        ))}
    </Modal>
  );
}

function More({ label, onClick }: { label: string; onClick: () => void }) {
  return (
    <Button size="sm" variant="ghost" iconRight="arrow-right" onClick={onClick}>
      {label}
    </Button>
  );
}

function healthTone(s?: SourceHealth["status"]) {
  return s === "healthy"
    ? "good"
    : s === "degraded"
      ? "warn"
      : s === "failing"
        ? "bad"
        : "neutral";
}
const healthRank = { failing: 0, degraded: 1, unknown: 2, healthy: 3 } as const;

function Empty({ children }: { children: string }) {
  return <p className="muted small ov-empty">{children}</p>;
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
    if (status.value) setChecked(when(new Date().toISOString()));
  }, [status.value]);
  const names = useNames(client);
  const feeds = useAllFeeds(client);
  const sources = useAllSources(client);
  const ops = useOpsMaps(client);
  const [range, setRange] = useState<keyof typeof ranges>("24h");
  const [tab, setTab] = useState("activity");
  const [cacheOpen, setCacheOpen] = useState(false);
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
    query: { limit: String(CAP), origin: "local" },
  });
  const audit = useOptionalCall(client, "listAuditEvents", {
    query: { limit: String(CAP) },
  });
  const replication = useOptionalCall(client, "listReplicationStatus");
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
    replication.reload();
  };
  const t = traffic.state === "ready" ? traffic.data : undefined;
  const st = storage.state === "ready" ? storage.data : undefined;
  const pressure = st?.pressure;
  const packages = [...ops.feeds.values()].reduce((n, f) => n + f.packages, 0);
  const hit = t?.totals.cache_hit_ratio;
  const topFeeds = [...feeds.items]
    .sort(
      (a, b) =>
        (ops.feeds.get(b.id)?.pulls ?? 0) - (ops.feeds.get(a.id)?.pulls ?? 0) ||
        a.slug.localeCompare(b.slug),
    )
    .slice(0, CAP);
  const topSources = [...sources.items]
    .sort(
      (a, b) =>
        healthRank[ops.health.get(a.id)?.status as keyof typeof healthRank] -
          healthRank[ops.health.get(b.id)?.status as keyof typeof healthRank] ||
        a.name.localeCompare(b.name),
    )
    .slice(0, CAP);
  const jobs = replication.state === "ready" ? replication.data.items : [];
  return (
    <div className="ov">
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
      <Failure error={feeds.error ?? sources.error} className="spaced" />
      <div className="ov-grid ov-grid--kpi">
        <Card
          className="ov-service"
          title="Service"
          actions={
            <>
              {status.value && (
                <Mono muted>
                  {status.value.data.name} · API {status.value.data.api_version}
                </Mono>
              )}
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
            </>
          }
        >
          <Failure error={status.error} />
          {status.loading ? (
            <Loading what="service status" />
          ) : (
            status.value && (
              <dl className="kv kv--compact">
                <dt className="ov-opt">Last check</dt>
                <dd className="ov-opt">{checked}</dd>
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
        <Kpi
          label="Packages"
          icon="package"
          value={ops.feeds.size ? count(packages) : "—"}
          sub={`${feeds.items.length} feeds · ${sources.items.length} sources`}
        />
        <Kpi
          label="Requests"
          icon="arrow-down-up"
          value={t ? count(t.totals.pulls + t.totals.publishes) : "—"}
          unit={t ? `/ ${range}` : undefined}
          sub={t ? "pulls + publishes" : "Metrics not available"}
        />
        <Kpi
          label="Cache hit ratio"
          icon="zap"
          value={hit != null ? (hit * 100).toFixed(1) : "—"}
          unit={hit != null ? "%" : undefined}
          sub={t ? `remote sources, ${range} · per remote` : "Metrics not available"}
          onClick={() => setCacheOpen(true)}
        />
        <Kpi
          label="Failing sources"
          icon="triangle-alert"
          value={String(failing.length)}
          sub={failing.length ? "Cached content still served" : "All reachable"}
        />
      </div>
      <div className="ov-grid ov-grid--mid">
        <Card
          className="ov-traffic"
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
                height={110}
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
          actions={
            <>
              {st && <Mono muted>{st.backend}</Mono>}
              <More label="Details" onClick={() => go("settings")} />
            </>
          }
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
              {pressure?.enabled && pressure.capacity_bytes > 0 && (
                <Meter
                  label={`Pressure (${pressure.state})`}
                  valueLabel={`${bytes(pressure.used_bytes)} / ${bytes(pressure.capacity_bytes)}`}
                  value={Math.min(100, Math.round(pressure.usage_ratio * 100))}
                />
              )}
              <p className="muted small ov-clamp">
                {count(st.blob_count)} blobs · dedup saved{" "}
                {bytes(st.dedup_saved_bytes)} · {bytes(st.unreferenced_bytes)}{" "}
                unreferenced ({count(st.gc_candidates)} collectable)
                {st.staging_bytes != null &&
                  ` · staging ${bytes(st.staging_bytes)}`}
              </p>
            </>
          )}
        </Card>
        <Card
          title="Replication"
          actions={<More label="View all" onClick={() => go("replication")} />}
        >
          {replication.state === "loading" && <Loading what="replication" />}
          {replication.state === "unavailable" && (
            <Unavailable what="Replication" />
          )}
          {replication.state === "error" && (
            <Failure error={replication.error} />
          )}
          {replication.state === "ready" &&
            (jobs.length ? (
              <Table
                onRowClick={() => go("replication")}
                columns={[
                  { key: "name", label: "Job" },
                  {
                    key: "state",
                    label: "State",
                    render: (r: ReplicationStatusEntry) => (
                      <Badge
                        dot
                        tone={
                          !r.enabled
                            ? "neutral"
                            : r.status.last_error
                              ? "bad"
                              : r.status.running
                                ? "info"
                                : "good"
                        }
                      >
                        {!r.enabled
                          ? "Paused"
                          : r.status.last_error
                            ? "Failing"
                            : r.status.running
                              ? "Running"
                              : r.status.phase.replace("_", " ")}
                      </Badge>
                    ),
                  },
                  {
                    key: "lag",
                    label: "Behind",
                    render: (r: ReplicationStatusEntry) =>
                      r.status.changes_behind != null
                        ? count(r.status.changes_behind)
                        : "—",
                  },
                ]}
                rows={jobs.slice(0, 3)}
              />
            ) : (
              <Empty>No replication jobs configured.</Empty>
            ))}
        </Card>
      </div>
      <div
        className={`ov-grid ov-grid--low${failing.length ? "" : " ov-grid--low3"}`}
      >
        <Card
          title="Feeds"
          actions={<More label="View all" onClick={() => go("feeds")} />}
        >
          {feeds.items.length ? (
            <Table
              onRowClick={(r: Feed) => go("feeds", { feed: r.id })}
              columns={[
                {
                  key: "slug",
                  label: "Feed",
                  render: (r: Feed) => <Mono>{r.slug}</Mono>,
                },
                { key: "format", label: "Format" },
                {
                  key: "packages",
                  label: "Pkgs",
                  render: (r: Feed) => {
                    const n = ops.feeds.get(r.id)?.packages;
                    return n == null ? "—" : count(n);
                  },
                },
              ]}
              rows={topFeeds}
            />
          ) : (
            <Empty>No feeds yet.</Empty>
          )}
        </Card>
        <Card
          title="Sources"
          actions={<More label="View all" onClick={() => go("sources")} />}
        >
          {topSources.length ? (
            <Table
              onRowClick={(r: Source) => go("sources", { source: r.id })}
              columns={[
                { key: "name", label: "Source" },
                { key: "kind", label: "Kind" },
                {
                  key: "health",
                  label: "Health",
                  render: (r: Source) => {
                    const h = ops.health.get(r.id)?.status;
                    return (
                      <Badge tone={healthTone(h)} dot>
                        {h && h !== "not_applicable" ? h : "n/a"}
                      </Badge>
                    );
                  },
                },
              ]}
              rows={topSources}
            />
          ) : (
            <Empty>No sources yet.</Empty>
          )}
        </Card>
        {failing.length > 0 && (
          <Card
            title="Failing sources"
            actions={
              <Badge tone={failing.length ? "bad" : "good"} dot>
                {failing.length ? failing.length : "None"}
              </Badge>
            }
          >
            {failing.length ? (
              <ul className="ov-list">
                {failing.slice(0, 3).map((s) => {
                  const h = ops.health.get(s.id)!;
                  return (
                    <li key={s.id}>
                      <div>
                        <b>{s.name}</b>
                        <small>
                          {h.last_status_code
                            ? `HTTP ${h.last_status_code} · `
                            : ""}
                          {h.last_error_at
                            ? `since ${when(h.last_error_at)}`
                            : "no successful check"}
                        </small>
                      </div>
                      <Button
                        size="sm"
                        variant="secondary"
                        onClick={() => go("sources", { source: s.id })}
                      >
                        Open source
                      </Button>
                    </li>
                  );
                })}
              </ul>
            ) : (
              <Empty>Every upstream source is reachable.</Empty>
            )}
            {failing.length > 3 && (
              <More
                label={`${failing.length - 3} more`}
                onClick={() => go("sources")}
              />
            )}
          </Card>
        )}
        <Card
          className="ov-recent"
          title="Recent"
          actions={
            <More
              label={tab === "activity" ? "Audit log" : "All packages"}
              onClick={() => go(tab === "activity" ? "audit" : "packages")}
            />
          }
        >
          <Tabs
            value={tab}
            onChange={setTab}
            className="ov-tabs"
            items={[
              { id: "activity", label: "Activity" },
              { id: "published", label: "Published" },
            ]}
          />
          {tab === "published" ? (
            recent.state === "ready" ? (
              <Table
                onRowClick={(r: PackageRef) => openPackage(r)}
                columns={[
                  {
                    key: "coordinate",
                    label: "Package",
                    render: (r: PackageRef) => (
                      <span className="ov-trunc">
                        {r.coordinate}
                        <small>{r.feed_slug}</small>
                      </span>
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
            )
          ) : audit.state === "ready" ? (
            <Table
              columns={[
                {
                  key: "action",
                  label: "Event",
                  render: (r: AuditEvent) => (
                    <span className="ov-trunc">
                      <Mono>{r.action}</Mono>
                      <small>
                        {r.principal_id
                          ? (names.get(r.principal_id) ??
                            shortId(r.principal_id))
                          : "anonymous"}
                        {" · "}
                        {when(r.created_at)}
                      </small>
                    </span>
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
        </Card>
      </div>
      {cacheOpen && (
        <CacheBreakdown
          client={client}
          go={go}
          range={range}
          from={window.from}
          to={window.to}
          onClose={() => setCacheOpen(false)}
        />
      )}
    </div>
  );
}
