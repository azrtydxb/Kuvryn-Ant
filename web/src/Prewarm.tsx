import { useEffect, useMemo, useRef, useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllFeeds } from "./data";
import { useOptionalCall } from "./gaps";
import type {
  CreatePrewarmJob,
  Feed,
  PrewarmFormat,
  PrewarmJob,
  PrewarmPreview,
  PrewarmRun,
  PrewarmRunItem,
  PrewarmSource,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Meter,
  Modal,
  PropertyList,
  Select,
  Switch,
  Table,
  Textarea,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Unavailable,
  bytes,
  chk,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

type Kind = PrewarmSource["kind"];

const KIND_LABEL: Record<Kind, string> = {
  packages: "Package list",
  lockfile: "Lockfile",
  images: "Image list",
  feed: "Another feed",
};

const SCHEDULES = [
  { value: "", label: "Only when started" },
  { value: "3600", label: "Every hour" },
  { value: "21600", label: "Every 6 hours" },
  { value: "86400", label: "Daily" },
  { value: "604800", label: "Weekly" },
];

const runTone = {
  queued: "info",
  running: "info",
  succeeded: "good",
  partial: "warn",
  failed: "bad",
  canceled: "neutral",
} as const;

const itemTone = {
  pending: "neutral",
  warmed: "good",
  cached: "info",
  failed: "bad",
  skipped: "warn",
} as const;

export const active = (r?: PrewarmRun) =>
  r?.state === "queued" || r?.state === "running";

// Reloads without clearing the view, while `again` says the data is still moving.
function useLive<T>(
  load: () => Promise<T>,
  deps: unknown[],
  again: (v: T) => boolean,
  interval = 3000,
) {
  const [data, setData] = useState<T>();
  const [error, setError] = useState<unknown>();
  const [version, setVersion] = useState(0);
  const loadRef = useRef(load);
  loadRef.current = load;
  const againRef = useRef(again);
  againRef.current = again;
  useEffect(() => {
    let live = true;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = () =>
      loadRef
        .current()
        .then((v) => {
          if (!live) return;
          setData(v);
          setError(undefined);
          if (againRef.current(v)) timer = setTimeout(tick, interval);
        })
        .catch((e) => {
          if (live) setError(e);
        });
    void tick();
    return () => {
      live = false;
      if (timer) clearTimeout(timer);
    };
  }, [...deps, version, interval]);
  return { data, error, reload: () => setVersion((v) => v + 1) };
}

export function PrewarmPane({ client }: { client: Client }) {
  const formats = useOptionalCall(client, "listPrewarmFormats");
  const feeds = useAllFeeds(client);
  const [feed, setFeed] = useState<string>();
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string>();
  const supported = useMemo(() => {
    if (formats.state !== "ready") return new Map<string, PrewarmFormat>();
    return new Map(formats.data.items.map((f) => [f.format, f]));
  }, [formats]);
  const candidates = feeds.items.filter((f) => supported.has(f.format));
  const selected = feed ?? candidates[0]?.id ?? "";
  const jobs = useLive(
    () =>
      client
        .call("listPrewarmJobs", {
          query: { limit: "100", ...(selected ? { feed_id: selected } : {}) },
        })
        .then((r) => r.data),
    [client, selected, feeds.loading],
    (d) => d.items.some((j) => active(j.last_run)),
  );
  if (formats.state === "loading" || feeds.loading)
    return <Loading what="prewarm" />;
  if (formats.state === "unavailable")
    return <Unavailable what="Cache prewarming" />;
  if (formats.state === "error") return <Failure error={formats.error} />;
  const slug = (id: string) => feeds.items.find((f) => f.id === id)?.slug ?? id;
  return (
    <>
      <Alert tone="info" title="Warm a feed's cache before it is needed">
        A job pulls packages through a feed that has a remote source, from a
        package list, a lockfile, an image list or everything another feed
        references, so builds find them cached. Items are fetched through the
        feed's normal routes, so ownership rules and integrity checks apply, and
        lockfile hashes are compared with the bytes. Supported:{" "}
        {[...supported.keys()].join(", ")}.
      </Alert>
      <div className="section-head">
        <h2>Prewarm jobs</h2>
        <div className="row">
          <Select
            aria-label="Feed"
            options={[
              ...candidates.map((f) => ({ value: f.id, label: f.slug })),
              { value: "", label: "All feeds (administrators)" },
            ]}
            value={selected}
            onChange={val(setFeed)}
          />
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            disabled={!candidates.length}
            onClick={() => setCreating(true)}
          >
            New job
          </Button>
        </div>
      </div>
      <Failure error={jobs.error} />
      {!candidates.length && (
        <p className="muted">
          No feed has a format that can be prewarmed. Supported formats:{" "}
          {[...supported.keys()].join(", ")}.
        </p>
      )}
      {jobs.data &&
        (jobs.data.items.length ? (
          <Table
            onRowClick={(j: PrewarmJob) => setOpen(j.id)}
            columns={[
              { key: "name", label: "Job" },
              {
                key: "feed",
                label: "Feed",
                render: (j: PrewarmJob) => <Mono>{slug(j.feed_id)}</Mono>,
              },
              {
                key: "source",
                label: "Source",
                render: (j: PrewarmJob) => (
                  <>
                    {KIND_LABEL[j.source_kind]}
                    <small>
                      {j.source_kind === "feed"
                        ? slug(j.source_feed_id ?? "")
                        : `${count(j.item_count)} items${j.lockfile_type ? ` · ${j.lockfile_type}` : ""}`}
                    </small>
                  </>
                ),
              },
              {
                key: "schedule",
                label: "Schedule",
                render: (j: PrewarmJob) =>
                  j.interval_seconds ? (
                    <>
                      {every(j.interval_seconds)}
                      <small>
                        {j.enabled
                          ? `next ${when(j.next_run_at)}`
                          : "paused (disabled)"}
                      </small>
                    </>
                  ) : (
                    "Manual"
                  ),
              },
              {
                key: "last",
                label: "Last run",
                render: (j: PrewarmJob) => <RunSummary run={j.last_run} />,
              },
            ]}
            rows={jobs.data.items}
          />
        ) : (
          <p className="muted">
            No prewarm jobs{selected ? " for this feed" : ""}.
          </p>
        ))}
      {creating && (
        <JobModal
          client={client}
          feeds={candidates}
          formats={supported}
          initialFeed={selected}
          onClose={() => setCreating(false)}
          done={(j) => {
            setCreating(false);
            jobs.reload();
            setOpen(j.id);
          }}
        />
      )}
      {open && (
        <JobDetail
          client={client}
          id={open}
          slug={slug}
          onClose={() => {
            setOpen(undefined);
            jobs.reload();
          }}
        />
      )}
    </>
  );
}

function every(seconds: number) {
  const named = SCHEDULES.find((s) => s.value === String(seconds));
  if (named) return named.label;
  if (seconds % 86400 === 0) return `Every ${seconds / 86400} days`;
  if (seconds % 3600 === 0) return `Every ${seconds / 3600} hours`;
  return `Every ${Math.round(seconds / 60)} minutes`;
}

function RunSummary({ run }: { run?: PrewarmRun }) {
  if (!run) return <span className="muted">Never run</span>;
  return (
    <>
      <Badge tone={runTone[run.state]} dot>
        {run.state}
      </Badge>
      <small>
        {when(run.finished_at ?? run.started_at ?? run.created_at)} ·{" "}
        {count(run.warmed)} warmed · {count(run.cached)} cached
        {run.failed ? ` · ${count(run.failed)} failed` : ""}
      </small>
    </>
  );
}

/* ---------- create ---------- */

function parsePackages(text: string, format: string) {
  const out: { name: string; version?: string }[] = [];
  for (const raw of text.split("\n")) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    if (format === "raw") {
      out.push({ name: line });
      continue;
    }
    const m =
      line.match(/^(\S+?)(?:\s*==\s*|\s+)(\S+)$/) ??
      line.match(/^(@?[^@\s]+)@(\S+)$/);
    out.push(m ? { name: m[1], version: m[2] } : { name: line });
  }
  return out;
}

function csv(text: string) {
  return text
    .split(",")
    .map((s) => s.trim())
    .filter(Boolean);
}

function JobModal({
  client,
  feeds,
  formats,
  initialFeed,
  onClose,
  done,
}: {
  client: Client;
  feeds: Feed[];
  formats: Map<string, PrewarmFormat>;
  initialFeed: string;
  onClose: () => void;
  done: (j: PrewarmJob) => void;
}) {
  const [feedId, setFeedId] = useState(initialFeed || feeds[0]?.id || "");
  const feed = feeds.find((f) => f.id === feedId);
  const info = feed ? formats.get(feed.format) : undefined;
  const kinds = (
    [
      info?.packages && "packages",
      info?.lockfiles.length && "lockfile",
      info?.images && "images",
      info?.feed && "feed",
    ].filter(Boolean) as Kind[]
  ).map((k) => ({ value: k, label: KIND_LABEL[k] }));
  const [name, setName] = useState("");
  const [kind, setKind] = useState<Kind>("packages");
  const [text, setText] = useState("");
  const [lockType, setLockType] = useState("");
  const [lockContent, setLockContent] = useState("");
  const [sourceFeed, setSourceFeed] = useState("");
  const [platforms, setPlatforms] = useState("");
  const [filters, setFilters] = useState("");
  const [concurrency, setConcurrency] = useState("4");
  const [rate, setRate] = useState("10");
  const [schedule, setSchedule] = useState("");
  const [runNow, setRunNow] = useState(true);
  const [preview, setPreview] = useState<PrewarmPreview>();
  const action = useAction();
  const previewing = useAction();
  const toast = useToast();
  const effectiveKind: Kind = kinds.some((k) => k.value === kind)
    ? kind
    : (kinds[0]?.value ?? "packages");
  const lockfiles = info?.lockfiles ?? [];
  const effectiveLock = lockfiles.includes(lockType)
    ? lockType
    : (lockfiles[0] ?? "");
  const sameFormat = feeds.filter(
    (f) => f.format === feed?.format && info?.feed,
  );
  const source = (): PrewarmSource => {
    switch (effectiveKind) {
      case "lockfile":
        return {
          kind: "lockfile",
          lockfile: {
            type: effectiveLock as never,
            content: lockContent,
          },
        };
      case "images":
        return {
          kind: "images",
          images: text
            .split("\n")
            .map((s) => s.trim())
            .filter(Boolean),
        };
      case "feed":
        return { kind: "feed", feed_id: sourceFeed || sameFormat[0]?.id };
      default:
        return {
          kind: "packages",
          packages: parsePackages(text, feed?.format ?? ""),
        };
    }
  };
  const options = () => {
    const o: { platforms?: string[]; file_filters?: string[] } = {};
    if (csv(platforms).length) o.platforms = csv(platforms);
    if (csv(filters).length) o.file_filters = csv(filters);
    return o;
  };
  return (
    <Modal
      title="New prewarm job"
      description="Warm the remote cache of a feed. The feed needs a remote source."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="secondary"
            disabled={previewing.busy || !feedId}
            onClick={async () => {
              setPreview(undefined);
              await previewing.run(async () => {
                setPreview(
                  (
                    await client.call("previewPrewarm", {
                      body: { feed_id: feedId, source: source() },
                    })
                  ).data,
                );
              });
            }}
          >
            {previewing.busy ? "Checking…" : "Preview"}
          </Button>
          <Button form="create-prewarm" type="submit" disabled={action.busy}>
            {action.busy ? "Saving…" : "Create job"}
          </Button>
        </>
      }
    >
      <form
        id="create-prewarm"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          let job: PrewarmJob | undefined;
          const ok = await action.run(async () => {
            const body: CreatePrewarmJob = {
              feed_id: feedId,
              name,
              source: source(),
              options: options(),
              concurrency: Number(concurrency),
              rate_per_second: Number(rate),
              run_now: runNow,
              ...(schedule ? { interval_seconds: Number(schedule) } : {}),
            };
            job = (await client.call("createPrewarmJob", { body })).data;
          });
          if (ok && job) {
            toast("Prewarm job created", name);
            done(job);
          }
        }}
      >
        <Select
          label="Feed"
          options={feeds.map((f) => ({
            value: f.id,
            label: `${f.slug} (${f.format})`,
          }))}
          value={feedId}
          onChange={val((v: string) => {
            setFeedId(v);
            setPreview(undefined);
          })}
        />
        <Input
          label="Name"
          required
          maxLength={200}
          value={name}
          onChange={val(setName)}
        />
        <Select
          label="Warm from"
          options={kinds}
          value={effectiveKind}
          onChange={val((v: string) => {
            setKind(v as Kind);
            setPreview(undefined);
          })}
        />
        {(effectiveKind === "packages" || effectiveKind === "images") && (
          <Textarea
            label={effectiveKind === "images" ? "Images" : "Packages"}
            mono
            rows={6}
            value={text}
            onChange={val(setText)}
            hint={
              effectiveKind === "images"
                ? "One per line: repository:tag or repository@sha256:digest, relative to the feed."
                : feed?.format === "raw"
                  ? "One path per line."
                  : feed?.format === "maven"
                    ? "One per line: group:artifact version (add :packaging:classifier after the artifact if needed)."
                    : "One per line: name version (or name@version, name==version)."
            }
          />
        )}
        {effectiveKind === "lockfile" && (
          <>
            <Select
              label="Lockfile type"
              options={lockfiles.map((l) => ({ value: l, label: l }))}
              value={effectiveLock}
              onChange={val(setLockType)}
            />
            <Textarea
              label="Lockfile content"
              mono
              rows={8}
              value={lockContent}
              onChange={val(setLockContent)}
              hint="Paste the file, or load it from disk. Entries that do not come from a registry (git, file and path dependencies) are ignored."
            />
            <input
              type="file"
              aria-label="Load lockfile from disk"
              onChange={async (e) => {
                const file = e.target.files?.[0];
                if (file) setLockContent(await file.text());
              }}
            />
          </>
        )}
        {effectiveKind === "feed" && (
          <Select
            label="Feed to read the package list from"
            options={sameFormat.map((f) => ({ value: f.id, label: f.slug }))}
            value={sourceFeed || sameFormat[0]?.id || ""}
            onChange={val(setSourceFeed)}
          />
        )}
        {feed?.format === "oci" && (
          <Input
            label="Platforms"
            value={platforms}
            onChange={val(setPlatforms)}
            placeholder="linux/amd64"
            hint="Comma separated os/arch[/variant], or all. Default linux/amd64."
          />
        )}
        {feed?.format === "pypi" && (
          <Input
            label="File filters"
            value={filters}
            onChange={val(setFilters)}
            hint="Comma separated. Only files whose name contains one of these are warmed. Default: pure-Python wheels and source distributions."
          />
        )}
        <div className="row">
          <Input
            label="Parallel items"
            type="number"
            min={1}
            max={32}
            value={concurrency}
            onChange={val(setConcurrency)}
          />
          <Input
            label="Items per second"
            type="number"
            min={0}
            max={1000}
            value={rate}
            onChange={val(setRate)}
            hint="0 is unlimited."
          />
        </div>
        <Select
          label="Schedule"
          options={SCHEDULES}
          value={schedule}
          onChange={val(setSchedule)}
        />
        <Switch
          label="Start a run now"
          labelPosition="end"
          checked={runNow}
          onChange={chk(setRunNow)}
        />
        {preview && (
          <Alert
            tone={preview.total ? "good" : "warn"}
            title={`${count(preview.total)} items would be warmed`}
          >
            {preview.items.slice(0, 5).map((i) => (
              <div key={`${i.name}@${i.version ?? ""}`}>
                <Mono>
                  {i.name}
                  {i.version ? ` ${i.version}` : ""}
                </Mono>
              </div>
            ))}
            {preview.ignored_count > 0 && (
              <p className="muted small">
                {count(preview.ignored_count)} entries ignored, for example{" "}
                {preview.ignored
                  .slice(0, 3)
                  .map((i) => `${i.name} (${i.reason})`)
                  .join(", ")}
                .
              </p>
            )}
          </Alert>
        )}
        <Failure error={action.error ?? previewing.error} />
      </form>
    </Modal>
  );
}

/* ---------- job detail ---------- */

function JobDetail({
  client,
  id,
  slug,
  onClose,
}: {
  client: Client;
  id: string;
  slug: (id: string) => string;
  onClose: () => void;
}) {
  const job = useLive(
    () => client.call("getPrewarmJob", { params: { id } }),
    [client, id],
    (r) => active(r.data.last_run),
  );
  const runs = useLive(
    () =>
      client
        .call("listPrewarmRuns", { query: { job_id: id, limit: "10" } })
        .then((r) => r.data),
    [client, id, job.data?.data.last_run?.id, job.data?.data.last_run?.state],
    (d) => d.items.some(active),
  );
  const [run, setRun] = useState<string>();
  const [confirm, setConfirm] = useState(false);
  const action = useAction();
  const toast = useToast();
  const j = job.data?.data;
  const etag = job.data?.etag;
  return (
    <Modal
      title={j ? j.name : "Prewarm job"}
      description={j ? `Feed ${slug(j.feed_id)}` : undefined}
      onClose={onClose}
      actions={
        j && (
          <>
            <Button
              variant="danger"
              icon="trash-2"
              disabled={action.busy}
              onClick={() => setConfirm(true)}
            >
              Delete
            </Button>
            <Button
              variant="secondary"
              disabled={action.busy}
              onClick={async () => {
                const ok = await action.run(async () => {
                  await client.call("patchPrewarmJob", {
                    params: { id },
                    etag: requireETag(etag ?? null),
                    body: { enabled: !j.enabled },
                  });
                });
                if (ok) job.reload();
              }}
            >
              {j.enabled ? "Disable" : "Enable"}
            </Button>
            <Button
              icon="play"
              disabled={action.busy || active(j.last_run)}
              onClick={async () => {
                const ok = await action.run(async () => {
                  await client.call("startPrewarmRun", {
                    params: { id },
                    body: {},
                  });
                });
                if (ok) {
                  toast("Prewarm run queued", j.name);
                  job.reload();
                }
              }}
            >
              Run now
            </Button>
          </>
        )
      }
    >
      <Failure error={job.error ?? action.error} />
      {!j && !job.error && <Loading what="job" />}
      {j && (
        <>
          <PropertyList
            items={[
              { label: "Source", value: sourceText(j, slug) },
              {
                label: "Schedule",
                value: j.interval_seconds
                  ? `${every(j.interval_seconds)}${j.enabled ? ` · next ${when(j.next_run_at)}` : " · paused"}`
                  : "Only when started",
              },
              {
                label: "Pace",
                value: `${j.concurrency} in parallel · ${j.rate_per_second ? `${j.rate_per_second}/s` : "unlimited"}`,
              },
              {
                label: "Options",
                value:
                  [
                    j.options.platforms &&
                      `platforms ${j.options.platforms.join(", ")}`,
                    j.options.file_filters &&
                      `files ${j.options.file_filters.join(", ")}`,
                  ]
                    .filter(Boolean)
                    .join(" · ") || "—",
              },
            ]}
          />
          <div className="section-head">
            <h3 className="h4">Recent runs</h3>
          </div>
          {runs.data &&
            (runs.data.items.length ? (
              <Table
                onRowClick={(r: PrewarmRun) => setRun(r.id)}
                columns={[
                  {
                    key: "created_at",
                    label: "Queued",
                    render: (r: PrewarmRun) => when(r.created_at),
                  },
                  { key: "trigger", label: "Trigger" },
                  {
                    key: "state",
                    label: "State",
                    render: (r: PrewarmRun) => (
                      <Badge tone={runTone[r.state]} dot>
                        {r.state}
                      </Badge>
                    ),
                  },
                  {
                    key: "progress",
                    label: "Items",
                    render: (r: PrewarmRun) =>
                      `${count(r.warmed + r.cached + r.failed + r.skipped)} / ${count(r.total)}`,
                  },
                  {
                    key: "failed",
                    label: "Failed",
                    align: "right",
                    render: (r: PrewarmRun) => count(r.failed),
                  },
                  {
                    key: "bytes",
                    label: "Read",
                    align: "right",
                    render: (r: PrewarmRun) => bytes(r.bytes),
                  },
                ]}
                rows={runs.data.items}
              />
            ) : (
              <p className="muted">No runs yet.</p>
            ))}
        </>
      )}
      {run && (
        <RunDetail
          client={client}
          id={run}
          onClose={() => {
            setRun(undefined);
            job.reload();
          }}
        />
      )}
      {confirm && j && (
        <Modal
          title="Delete prewarm job"
          description="The job and its run history are removed and a run in progress stops. Content that was already cached stays."
          onClose={() => setConfirm(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setConfirm(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={action.busy}
                onClick={async () => {
                  const ok = await action.run(async () => {
                    await client.call("deletePrewarmJob", {
                      params: { id },
                      etag: requireETag(etag ?? null),
                    });
                  });
                  if (ok) {
                    toast("Prewarm job deleted", j.name);
                    setConfirm(false);
                    onClose();
                  }
                }}
              >
                Delete job
              </Button>
            </>
          }
        >
          <p>
            Delete <b>{j.name}</b>?
          </p>
        </Modal>
      )}
    </Modal>
  );
}

function sourceText(j: PrewarmJob, slug: (id: string) => string) {
  switch (j.source_kind) {
    case "feed":
      return `Everything referenced by feed ${slug(j.source_feed_id ?? "")}`;
    case "lockfile":
      return `${j.lockfile_type} · ${count(j.item_count)} packages`;
    case "images":
      return `${count(j.item_count)} images`;
    default:
      return `${count(j.item_count)} packages`;
  }
}

/* ---------- run detail ---------- */

const FAILURES: Record<string, string> = {
  not_found: "Not found upstream",
  upstream_unavailable: "Upstream unavailable",
  integrity_mismatch: "Bytes do not match the pinned hash",
  denied: "Not permitted for the run's principal",
  timeout: "Timed out",
  internal: "Internal error",
  canceled: "Canceled",
  no_matching_file: "No matching file to warm",
  unsupported: "Not supported",
};

function RunDetail({
  client,
  id,
  onClose,
}: {
  client: Client;
  id: string;
  onClose: () => void;
}) {
  const [state, setState] = useState("failed");
  const run = useLive(
    () => client.call("getPrewarmRun", { params: { id } }).then((r) => r.data),
    [client, id],
    active,
    2000,
  );
  const items = useLive(
    () =>
      client
        .call("listPrewarmRunItems", {
          params: { id },
          query: { limit: "100", ...(state ? { state } : {}) },
        })
        .then((r) => r.data),
    [client, id, state, run.data?.state],
    () => active(run.data),
    2000,
  );
  const action = useAction();
  const toast = useToast();
  const r = run.data;
  const done = r ? r.warmed + r.cached + r.failed + r.skipped : 0;
  return (
    <Modal
      title={r ? `Run · ${r.state}` : "Prewarm run"}
      description={r ? `Queued ${when(r.created_at)}` : undefined}
      onClose={onClose}
      actions={
        r && (
          <>
            {active(r) ? (
              <Button
                variant="secondary"
                icon="x"
                disabled={action.busy || r.cancel_requested}
                onClick={async () => {
                  const ok = await action.run(async () => {
                    await client.call("cancelPrewarmRun", { params: { id } });
                  });
                  if (ok) {
                    toast("Cancel requested");
                    run.reload();
                  }
                }}
              >
                {r.cancel_requested ? "Canceling…" : "Cancel run"}
              </Button>
            ) : (
              <Button
                variant="secondary"
                icon="rotate-ccw"
                disabled={
                  action.busy ||
                  r.failed + r.skipped === 0 ||
                  r.state === "succeeded"
                }
                onClick={async () => {
                  const ok = await action.run(async () => {
                    await client.call("retryPrewarmRun", { params: { id } });
                  });
                  if (ok) {
                    toast("Retry queued", "Failed and unfinished items");
                    onClose();
                  }
                }}
              >
                Retry failed items
              </Button>
            )}
            <Button onClick={onClose}>Done</Button>
          </>
        )
      }
    >
      <Failure error={run.error ?? action.error} />
      {!r && !run.error && <Loading what="run" />}
      {r && (
        <>
          <Meter
            label="Progress"
            valueLabel={`${count(done)} / ${count(r.total)} items`}
            value={r.total ? Math.round((done / r.total) * 100) : 0}
          />
          <PropertyList
            items={[
              {
                label: "Warmed (fetched from upstream)",
                value: count(r.warmed),
              },
              { label: "Already cached", value: count(r.cached) },
              { label: "Failed", value: count(r.failed) },
              { label: "Skipped", value: count(r.skipped) },
              { label: "Bytes read", value: bytes(r.bytes) },
              {
                label: "Finished",
                value: r.finished_at ? when(r.finished_at) : "—",
              },
              ...(r.error ? [{ label: "Note", value: r.error }] : []),
            ]}
          />
          <div className="section-head">
            <h3 className="h4">Items</h3>
            <Select
              aria-label="Item state"
              options={[
                { value: "", label: "All" },
                { value: "failed", label: "Failed" },
                { value: "skipped", label: "Skipped" },
                { value: "warmed", label: "Warmed" },
                { value: "cached", label: "Cached" },
                { value: "pending", label: "Pending" },
              ]}
              value={state}
              onChange={val(setState)}
            />
          </div>
          {items.data &&
            (items.data.items.length ? (
              <Table
                rowKey="seq"
                columns={[
                  {
                    key: "name",
                    label: "Item",
                    render: (i: PrewarmRunItem) => (
                      <Mono>
                        {i.name}
                        {i.version ? ` ${i.version}` : ""}
                      </Mono>
                    ),
                  },
                  {
                    key: "state",
                    label: "State",
                    render: (i: PrewarmRunItem) => (
                      <>
                        <Badge tone={itemTone[i.state]} dot>
                          {i.state}
                        </Badge>
                        {i.error && (
                          <small>{FAILURES[i.error] ?? i.error}</small>
                        )}
                      </>
                    ),
                  },
                  {
                    key: "attempts",
                    label: "Attempts",
                    align: "right",
                  },
                  {
                    key: "bytes",
                    label: "Read",
                    align: "right",
                    render: (i: PrewarmRunItem) => bytes(i.bytes),
                  },
                ]}
                rows={items.data.items}
              />
            ) : (
              <p className="muted">No items in this state.</p>
            ))}
          {items.data?.next_after !== undefined && (
            <p className="muted small">
              Showing the first {items.data.items.length} items.
            </p>
          )}
        </>
      )}
    </Modal>
  );
}
