import { useState } from "react";
import { requireETag, type Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  CacheFeedUsage,
  PressureReport,
  PressureStatus,
  RetentionSettings,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Meter,
  PropertyList,
  Switch,
  Table,
} from "./ds/bundle";
import { CacheBadge, idleLabel } from "./CachePolicy";
import {
  Failure,
  Loading,
  Unavailable,
  bytes,
  chk,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

const DAY = 86400;
const GIB = 2 ** 30;
const days = (s?: number) => (s ? String(Math.round(s / DAY)) : "");

const stateTone = {
  disabled: "neutral",
  no_capacity: "warn",
  invalid: "bad",
  ok: "good",
  pressure: "warn",
  unevictable: "bad",
} as const;
const stateLabel = {
  disabled: "Disabled",
  no_capacity: "No capacity known",
  invalid: "Invalid settings",
  ok: "Below high watermark",
  pressure: "Above high watermark",
  unevictable: "Nothing evictable",
} as const;

export function PressureBadge({ state }: { state: PressureStatus["state"] }) {
  return (
    <Badge tone={stateTone[state] ?? "neutral"} dot>
      {stateLabel[state] ?? state}
    </Badge>
  );
}

/** Usage against capacity, watermarks and reclaimable bytes, with run buttons. */
export function OverviewTab({
  client,
  onRun,
}: {
  client: Client;
  onRun: (mode: "dry" | "live") => void;
}) {
  const usage = useOptionalCall(client, "getStorageUsage");
  if (usage.state === "loading") return <Loading what="storage usage" />;
  if (usage.state === "unavailable")
    return <Unavailable what="Storage usage" />;
  if (usage.state === "error") return <Failure error={usage.error} />;
  const u = usage.data;
  const p = u.pressure;
  const ratio = p && p.capacity_bytes > 0 ? p.usage_ratio : undefined;
  return (
    <>
      <div className="section-head">
        <h2>Storage pressure</h2>
        <div className="row-actions">
          <Button
            size="sm"
            variant="secondary"
            icon="eye"
            onClick={() => onRun("dry")}
          >
            Dry run
          </Button>
          <Button size="sm" icon="play" onClick={() => onRun("live")}>
            Run now
          </Button>
        </div>
      </div>
      {p?.state === "no_capacity" && (
        <Alert tone="warn" title="Pressure eviction is off for this backend">
          Object storage has no native capacity. Set a capacity quota in
          Defaults to enable the high and low watermarks.
        </Alert>
      )}
      {p?.state === "unevictable" && (
        <Alert tone="danger" title="Storage is full and nothing is evictable">
          Usage is above the high watermark. Remote cache content is exhausted
          and local or replica content is never evicted unless its policy allows
          release deletion. Add capacity or allow release deletion.
        </Alert>
      )}
      {p && !p.enabled && (
        <Alert tone="info" title="Pressure eviction is disabled">
          Enable it under Defaults to evict least-recently-used cached content
          when storage fills.
        </Alert>
      )}
      <div className="stack">
        <div>
          {p && <PressureBadge state={p.state} />}{" "}
          {p?.detail && <span className="muted small">{p.detail}</span>}
        </div>
        {ratio !== undefined && p ? (
          <Meter
            label="Usage"
            valueLabel={`${bytes(p.used_bytes)} of ${bytes(p.capacity_bytes)} (${(ratio * 100).toFixed(1)}%) · high ${p.high_percent}% · low ${p.low_percent}%`}
            value={Math.min(100, Math.round(ratio * 100))}
          />
        ) : (
          <p className="muted">Capacity is not known for this backend.</p>
        )}
        <PropertyList
          items={[
            { label: "Backend", value: u.backend },
            {
              label: "Capacity source",
              value:
                p?.capacity_source === "filesystem"
                  ? "Filesystem of the blob root"
                  : p?.capacity_source === "quota"
                    ? "Configured quota"
                    : "None",
            },
            { label: "Stored blobs", value: bytes(u.blob_bytes) },
            {
              label: "Unreferenced",
              value: `${bytes(u.unreferenced_bytes)} (${bytes(u.reclaimable_bytes)} reclaimable · ${bytes(u.in_grace_bytes)} still in the grace period)`,
            },
            { label: "GC candidates", value: count(u.gc_candidates) },
            {
              label: "Last eviction",
              value: p?.last_evicted_at
                ? `${when(p.last_evicted_at)} · ${count(p.last_evicted_versions)} versions · ${bytes(p.last_evicted_bytes)}`
                : "Never",
            },
            {
              label: "Evicted in total",
              value: `${count(p?.total_evicted_versions)} versions · ${bytes(p?.total_evicted_bytes)}`,
            },
            { label: "Last check", value: when(p?.checked_at) },
          ]}
        />
        <p className="muted small">
          Unreferenced blobs are only deleted after the grace period, in two
          garbage-collection runs. Blobs freed by pressure eviction wait only a
          short while.
        </p>
      </div>
      <CacheFeedsSection feeds={u.cache_feeds ?? []} />
    </>
  );
}

/** Bytes held by each cache feed. Cache feeds are evicted first under pressure. */
export function CacheFeedsSection({ feeds }: { feeds: CacheFeedUsage[] }) {
  if (!feeds.length) return null;
  const total = feeds.reduce((n, f) => n + f.bytes, 0);
  return (
    <>
      <div className="section-head">
        <h2>Build caches</h2>
        <span className="muted small">
          {feeds.length} cache feed{feeds.length === 1 ? "" : "s"} ·{" "}
          {bytes(total)}
        </span>
      </div>
      <Table
        columns={[
          {
            key: "slug",
            label: "Cache feed",
            render: (f: CacheFeedUsage) => (
              <>
                <span className="mono">{f.slug}</span> <CacheBadge />
              </>
            ),
          },
          { key: "format", label: "Format" },
          {
            key: "bytes",
            label: "Stored",
            align: "right",
            render: (f: CacheFeedUsage) => bytes(f.bytes),
          },
          {
            key: "max_bytes",
            label: "Size limit",
            align: "right",
            render: (f: CacheFeedUsage) =>
              f.max_bytes ? bytes(f.max_bytes) : "No limit",
          },
          {
            key: "max_idle_seconds",
            label: "Evicted when idle",
            align: "right",
            render: (f: CacheFeedUsage) => idleLabel(f.max_idle_seconds),
          },
          {
            key: "versions",
            label: "Versions",
            align: "right",
            render: (f: CacheFeedUsage) => count(f.versions),
          },
        ]}
        rows={feeds}
      />
      <p className="muted small">
        Cache feeds are evicted before any other content when storage fills,
        least recently used first, and need no release-deletion policy.
      </p>
    </>
  );
}

/** Pressure report section of a run. */
export function PressureSummary({ report }: { report?: PressureReport }) {
  if (!report || !report.state) return null;
  return (
    <>
      <h3>Storage pressure</h3>
      <PropertyList
        items={[
          { label: "State", value: <PressureBadge state={report.state} /> },
          {
            label: "Usage",
            value: report.capacity_bytes
              ? `${bytes(report.used_bytes)} of ${bytes(report.capacity_bytes)} (${(report.usage_ratio * 100).toFixed(1)}%)`
              : "Capacity unknown",
          },
          {
            label: "Needed to reach low watermark",
            value: bytes(report.target_bytes),
          },
          {
            label: report.dry_run ? "Would evict" : "Evicted",
            value: `${count(report.evicted_versions)} versions · ${bytes(report.evicted_bytes)}`,
          },
          { label: "Shortfall", value: bytes(report.shortfall_bytes) },
          {
            label: "Sweep shortened / swept",
            value: `${count(report.expedited)} / ${count(report.swept)}`,
          },
        ]}
      />
      {report.detail && <p className="muted small">{report.detail}</p>}
    </>
  );
}

/** Instance default policy and watermarks. */
export function DefaultsTab({ client }: { client: Client }) {
  const settings = useOptionalCall(client, "getRetentionSettings");
  if (settings.state === "loading")
    return <Loading what="retention settings" />;
  if (settings.state === "unavailable")
    return <Unavailable what="Retention settings" />;
  if (settings.state === "error") return <Failure error={settings.error} />;
  if (!settings.data.default_policy || !settings.data.pressure)
    return <Unavailable what="Retention settings" />;
  return (
    <DefaultsForm
      key={settings.data.revision}
      client={client}
      settings={settings.data}
      reload={settings.reload}
    />
  );
}

function DefaultsForm({
  client,
  settings,
  reload,
}: {
  client: Client;
  settings: RetentionSettings;
  reload: () => void;
}) {
  const d = settings.default_policy;
  const p = settings.pressure;
  const locked = (f: string) => settings.locked.includes(f);
  const [enabled, setEnabled] = useState(d.enabled);
  const [idle, setIdle] = useState(days(d.max_idle_seconds));
  const [age, setAge] = useState(days(d.max_age_seconds));
  const [keep, setKeep] = useState(String(d.keep_last ?? ""));
  const [min, setMin] = useState(String(d.min_keep));
  const [tagged, setTagged] = useState(d.include_tagged);
  const [pEnabled, setPEnabled] = useState(p.enabled);
  const [high, setHigh] = useState(String(p.high_percent));
  const [low, setLow] = useState(String(p.low_percent));
  const [cap, setCap] = useState(
    p.capacity_bytes ? String(+(p.capacity_bytes / GIB).toFixed(2)) : "",
  );
  const action = useAction();
  const toast = useToast();
  const num = (v: string) => (v === "" ? undefined : Number(v));
  const note = (f: string) =>
    locked(f) ? "Set by the deployment environment" : undefined;
  const save = async () => {
    const ok = await action.run(() =>
      client.call("putRetentionSettings", {
        etag: requireETag(`"${settings.revision}"`),
        body: {
          default_policy: {
            enabled,
            keep_last: num(keep),
            max_age_seconds: age === "" ? undefined : Number(age) * DAY,
            max_idle_seconds: idle === "" ? undefined : Number(idle) * DAY,
            min_keep: Number(min || 0),
            include_tagged: tagged,
          },
          pressure: {
            enabled: pEnabled,
            high_percent: Number(high),
            low_percent: Number(low),
            capacity_bytes:
              cap === "" ? undefined : Math.round(Number(cap) * GIB),
          },
        },
      }),
    );
    if (ok) {
      toast("Retention settings saved");
      reload();
    }
  };
  return (
    <>
      <div className="section-head">
        <h2>Default policy</h2>
      </div>
      <p className="muted">
        Applies to cached remote content in every feed that has no policy of its
        own. Local and replica content is never deleted by it.
      </p>
      <Switch
        label="Enable the default policy"
        labelPosition="end"
        checked={enabled}
        disabled={locked("default_policy.enabled")}
        onChange={chk(setEnabled)}
      />
      <div className="cols">
        <Input
          label="Delete when not accessed for (days)"
          type="number"
          mono
          min={1}
          value={idle}
          disabled={locked("default_policy.max_idle_seconds")}
          hint={note("default_policy.max_idle_seconds")}
          onChange={val(setIdle)}
        />
        <Input
          label="Delete when older than (days)"
          type="number"
          mono
          min={1}
          value={age}
          disabled={locked("default_policy.max_age_seconds")}
          hint={note("default_policy.max_age_seconds")}
          onChange={val(setAge)}
        />
        <Input
          label="Keep last (versions)"
          type="number"
          mono
          min={1}
          value={keep}
          disabled={locked("default_policy.keep_last")}
          hint={note("default_policy.keep_last")}
          onChange={val(setKeep)}
        />
        <Input
          label="Min keep"
          type="number"
          mono
          min={0}
          value={min}
          onChange={val(setMin)}
        />
      </div>
      <Switch
        label="Include tagged versions"
        labelPosition="end"
        checked={tagged}
        onChange={chk(setTagged)}
      />
      <div className="section-head">
        <h2>Storage pressure</h2>
      </div>
      <p className="muted">
        When usage reaches the high watermark, least-recently-used remote cache
        content is evicted until usage is projected at or below the low
        watermark. Min keep and tagged versions are respected.
      </p>
      <Switch
        label="Evict cached content under storage pressure"
        labelPosition="end"
        checked={pEnabled}
        disabled={locked("pressure.enabled")}
        onChange={chk(setPEnabled)}
      />
      <div className="cols">
        <Input
          label="High watermark (%)"
          type="number"
          mono
          min={2}
          max={99}
          value={high}
          disabled={locked("pressure.high_percent")}
          hint={note("pressure.high_percent")}
          onChange={val(setHigh)}
        />
        <Input
          label="Low watermark (%)"
          type="number"
          mono
          min={1}
          max={98}
          value={low}
          disabled={locked("pressure.low_percent")}
          hint={note("pressure.low_percent")}
          onChange={val(setLow)}
        />
        <Input
          label="Capacity quota (GiB)"
          type="number"
          mono
          min={0}
          value={cap}
          disabled={locked("pressure.capacity_bytes")}
          hint={
            note("pressure.capacity_bytes") ??
            "Required for object storage; for disk it overrides the filesystem size."
          }
          onChange={val(setCap)}
        />
      </div>
      <Failure error={action.error} />
      <div className="section-head">
        <Button disabled={action.busy} onClick={save}>
          {action.busy ? "Saving…" : "Save settings"}
        </Button>
      </div>
      <h3>Schedule</h3>
      <PropertyList
        items={[
          {
            label: "Maintenance interval",
            value: seconds(settings.gc.interval_seconds),
          },
          {
            label: "Orphan blob grace",
            value: seconds(settings.gc.grace_seconds),
          },
          {
            label: "Pressure check interval",
            value: seconds(settings.gc.pressure_interval_seconds),
          },
          {
            label: "Pressure sweep wait",
            value: seconds(settings.gc.pressure_sweep_grace_seconds),
          },
        ]}
      />
      <p className="muted small">
        The schedule comes from the deployment environment (ANT_GC_INTERVAL,
        ANT_GC_GRACE, ANT_PRESSURE_INTERVAL, ANT_PRESSURE_SWEEP_GRACE).
      </p>
    </>
  );
}

function seconds(s: number) {
  if (!s) return "Disabled";
  if (s % 86400 === 0) return `${s / 86400} d`;
  if (s % 3600 === 0) return `${s / 3600} h`;
  if (s % 60 === 0) return `${s / 60} min`;
  return `${s} s`;
}
