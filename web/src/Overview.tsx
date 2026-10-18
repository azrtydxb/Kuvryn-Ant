import { useEffect, useState } from "react";
import type { Client } from "./api";
import type { Go } from "./App";
import { useAllFeeds, useAllSources } from "./data";
import {
  paths,
  useOptional,
  type PackageSearchPage,
  type AuditRecord,
  type Readiness,
  type SourceStats,
  type Storage,
  type Traffic,
} from "./gaps";
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
import type { PackageRef } from "./Packages";
import {
  Card,
  Failure,
  Loading,
  Mono,
  Needs,
  PageHeading,
  SectionHead,
  Unavailable,
  bytes,
  count,
  useResource,
  when,
} from "./ui";

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
  const [range, setRange] = useState("24h");
  const traffic = useOptional<Traffic>(client, paths.traffic, { range });
  const storage = useOptional<Storage>(client, paths.storage);
  const recent = useOptional<PackageSearchPage>(client, paths.packageSearch, {
    limit: "4",
    origin: "local",
  });
  const audit = useOptional<{ items: AuditRecord[] }>(client, paths.audit, {
    limit: "4",
  });
  const readiness = (status.value?.data as Readiness | undefined)?.readiness;
  const failing = sources.items.filter(
    (s) => (s as SourceStats).health?.state === "failing",
  );
  const reload = () => {
    status.reload();
    feeds.reload();
    sources.reload();
    traffic.reload();
    storage.reload();
    recent.reload();
    audit.reload();
  };
  const t = traffic.state === "ready" ? traffic.data : undefined;
  const st = storage.state === "ready" ? storage.data : undefined;
  const pct = (used?: number, cap?: number) =>
    used != null && cap ? Math.min(100, Math.round((used / cap) * 100)) : 0;
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
      {failing.map((s) => (
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
          {(s as SourceStats).health?.message ??
            "The last upstream request failed."}
        </Alert>
      ))}
      <div className="grid-auto grid-auto--210">
        <Card
          title="Service"
          actions={
            <Badge tone={status.error ? "bad" : "good"} dot>
              {status.error ? "Unreachable" : "Reachable"}
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
                {readiness?.database && (
                  <>
                    <dt>PostgreSQL</dt>
                    <dd>
                      <Badge
                        tone={readiness.database.ready ? "good" : "bad"}
                        dot
                      >
                        {readiness.database.ready ? "Ready" : "Not ready"}
                        {readiness.database.latency_ms != null &&
                          ` · ${readiness.database.latency_ms} ms`}
                      </Badge>
                    </dd>
                  </>
                )}
                {readiness?.storage && (
                  <>
                    <dt>Blob storage</dt>
                    <dd>
                      <Badge
                        tone={readiness.storage.ready ? "good" : "bad"}
                        dot
                      >
                        {readiness.storage.ready ? "Ready" : "Not ready"}
                      </Badge>
                    </dd>
                  </>
                )}
                {readiness?.writer_lease && (
                  <>
                    <dt>Writer lease</dt>
                    <dd>
                      <Badge
                        tone={readiness.writer_lease.held ? "good" : "warn"}
                        dot
                      >
                        {readiness.writer_lease.held ? "Held" : "Not held"}
                      </Badge>
                    </dd>
                  </>
                )}
              </dl>
            )
          )}
          {status.value && !readiness && (
            <p className="muted small">
              Reachability does not establish database, storage or writer lease
              readiness. Readiness detail: Not available on this server yet.
            </p>
          )}
          <Needs>readiness detail</Needs>
        </Card>
        <StatCard
          label="Feeds"
          value={feeds.loading ? "…" : count(feeds.items.length)}
          icon="rss"
          sub="visible to you"
        />
        <StatCard
          label="Sources"
          value={sources.loading ? "…" : count(sources.items.length)}
          icon="database"
          sub={`${sources.items.filter((s) => s.kind === "remote").length} remote · ${sources.items.filter((s) => s.kind === "local").length} local`}
        />
        <StatCard
          label="Requests"
          value={
            t?.totals
              ? count((t.totals.pulls ?? 0) + (t.totals.publishes ?? 0))
              : "—"
          }
          unit={t?.totals ? `/ ${range}` : undefined}
          icon="arrow-down-up"
          sub={t ? "pulls + publishes" : "Metrics not available yet"}
        />
        <StatCard
          label="Cache hit ratio"
          value={
            t?.cache_hit_ratio != null ? t.cache_hit_ratio.toFixed(1) : "—"
          }
          unit={t?.cache_hit_ratio != null ? "%" : undefined}
          icon="zap"
          sub={t ? `remote sources, ${range}` : "Metrics not available yet"}
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
              options={["1h", "24h", "7d"]}
              value={range}
              onChange={setRange}
            />
          }
        >
          {traffic.state === "loading" && <Loading what="traffic" />}
          {traffic.state === "unavailable" && (
            <Unavailable what="Traffic metrics (currently Prometheus only)" />
          )}
          {traffic.state === "error" && <Failure error={traffic.error} />}
          {t && (
            <>
              <LineChart
                height={170}
                series={[{ data: t.pulls ?? [] }, { data: t.publishes ?? [] }]}
                labels={t.labels ?? []}
              />
              <ChartLegend
                items={[
                  { label: "Pulls", value: count(t.totals?.pulls) },
                  { label: "Publishes", value: count(t.totals?.publishes) },
                ]}
              />
            </>
          )}
          <Needs>metrics query (currently Prometheus only)</Needs>
        </Card>
        <Card
          title="Storage"
          actions={st?.endpoint ? <Mono muted>{st.endpoint}</Mono> : undefined}
        >
          {storage.state === "loading" && <Loading what="storage usage" />}
          {storage.state === "unavailable" && (
            <Unavailable what="Storage usage" />
          )}
          {storage.state === "error" && <Failure error={storage.error} />}
          {st && (
            <>
              <Meter
                label="Bucket usage"
                valueLabel={`${bytes(st.bucket_used_bytes)} / ${bytes(st.bucket_capacity_bytes)}`}
                value={pct(st.bucket_used_bytes, st.bucket_capacity_bytes)}
                tone="auto"
              />
              <Meter
                label="Upload staging (local disk)"
                valueLabel={`${bytes(st.staging_used_bytes)} / ${bytes(st.staging_capacity_bytes)}`}
                value={pct(st.staging_used_bytes, st.staging_capacity_bytes)}
                tone="auto"
              />
              <p className="muted small">
                {bytes(st.local_bytes)} local · {bytes(st.cached_bytes)} cached
                {st.dedup_saved_bytes != null &&
                  ` · deduplication saved ${bytes(st.dedup_saved_bytes)}`}
              </p>
            </>
          )}
          <Needs>storage usage</Needs>
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
                { key: "latest_version", label: "Version", mono: true },
                {
                  key: "updated_at",
                  label: "Published",
                  align: "right",
                  render: (r: PackageRef) => when(r.updated_at),
                },
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
                  render: (r: AuditRecord) => (
                    <>
                      <Mono>{r.action}</Mono>
                      <small>{r.principal_subject ?? r.principal_id}</small>
                    </>
                  ),
                },
                {
                  key: "result",
                  label: "Result",
                  render: (r: AuditRecord) => (
                    <Badge tone={r.result === "denied" ? "bad" : "good"} dot>
                      {r.result}
                    </Badge>
                  ),
                },
                {
                  key: "at",
                  label: "Time",
                  align: "right",
                  render: (r: AuditRecord) => when(r.at),
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
