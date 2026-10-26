import { useEffect, useState } from "react";
import type { Go } from "./App";
import { requireETag, type Client } from "./api";
import { useAllSources, useOpsMaps } from "./data";
import { isUnavailable } from "./gaps";
import { HealthBadge } from "./Health";
import {
  formats,
  type Source,
  type SourceHealth,
  type SourceStats,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Drawer,
  EmptyState,
  Icon,
  Input,
  Modal,
  PropertyList,
  Select,
  Table,
  Tabs,
} from "./ds/bundle";
import { RemoteSource } from "./RemoteSource";
import { SourceIcon } from "./SourceIcon";
import {
  UpstreamFields,
  createRemoteSource,
  draftProblem,
  emptyDraft,
  type UpstreamDraft,
} from "./UpstreamFields";
import { iconForSource } from "./upstreams";
import {
  Copyable,
  Failure,
  Loading,
  PageHeading,
  Toolbar,
  Unavailable,
  bytes,
  count,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

export function SourcesPage({
  client,
  openId,
  go,
}: {
  client: Client;
  openId: string;
  go?: Go;
}) {
  const all = useAllSources(client);
  const ops = useOpsMaps(client);
  const [kind, setKind] = useState("all");
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<string>(openId);
  const [creating, setCreating] = useState(false);
  const [fresh, setFresh] = useState("");
  useEffect(() => setOpen(openId), [openId]);
  const items = all.items;
  const rows = items.filter(
    (s) =>
      (kind === "all" || s.kind === kind) &&
      s.name.toLowerCase().includes(filter.trim().toLowerCase()),
  );
  const nOf = (k: string) => items.filter((s) => s.kind === k).length;
  const current = items.find((s) => s.id === open);
  return (
    <>
      <PageHeading
        eyebrow="Content origins"
        title="Sources"
        text="Where content lives: local storage, cached upstream registries, or replicas created by Replication. Attach sources to feeds of the same format."
      >
        <Button icon="plus" onClick={() => setCreating(true)}>
          Create source
        </Button>
      </PageHeading>
      <Toolbar right={`${rows.length} source${rows.length === 1 ? "" : "s"}`}>
        <Input
          icon="search"
          size="sm"
          aria-label="Filter by source name"
          placeholder="Filter by name"
          maxLength={200}
          value={filter}
          onChange={val(setFilter)}
          className="w240"
        />
        <Tabs
          value={kind}
          onChange={setKind}
          items={[
            { id: "all", label: "All", count: items.length },
            { id: "local", label: "Local", count: nOf("local") },
            { id: "remote", label: "Remote", count: nOf("remote") },
            { id: "replica", label: "Replica", count: nOf("replica") },
          ]}
        />
        <Button
          size="sm"
          variant="secondary"
          icon="refresh-cw"
          disabled={all.loading}
          onClick={all.reload}
        >
          Refresh
        </Button>
      </Toolbar>
      <Failure error={all.error} />
      {all.loading && <Loading what="sources" />}
      {!all.loading &&
        !all.error &&
        (rows.length ? (
          <Table
            onRowClick={(s: Source) => setOpen(s.id)}
            columns={[
              {
                key: "name",
                label: "Source",
                render: (s: Source) => (
                  <button
                    type="button"
                    className="rowlink"
                    onClick={(e) => {
                      e.stopPropagation();
                      setOpen(s.id);
                    }}
                  >
                    <span className="titled">
                      <SourceIcon source={s} size="md" />
                      <span>
                        {s.name}
                        <small>
                          {s.kind === "remote"
                            ? (iconForSource(s).upstream?.name ??
                              "Remote upstream")
                            : s.kind === "local"
                              ? "Local storage"
                              : "Replica"}
                        </small>
                      </span>
                    </span>
                  </button>
                ),
              },
              {
                key: "kind",
                label: "Kind",
                render: (s: Source) => (
                  <span className="row tight">
                    <Icon
                      name={
                        s.kind === "remote"
                          ? "cloud-download"
                          : s.kind === "replica"
                            ? "copy"
                            : "hard-drive"
                      }
                      size={14}
                    />
                    {s.kind}
                  </span>
                ),
              },
              {
                key: "format",
                label: "Format",
                render: (s: Source) => <Badge tone="outline">{s.format}</Badge>,
              },
              {
                key: "feeds",
                label: "Used by",
                align: "right",
                render: (s: Source) => {
                  const n = ops.sources.get(s.id)?.attached_feeds;
                  return n == null ? "—" : `${n} ${n === 1 ? "feed" : "feeds"}`;
                },
              },
              {
                key: "size",
                label: "Stored",
                align: "right",
                render: (s: Source) => bytes(ops.sources.get(s.id)?.bytes),
              },
              {
                key: "health",
                label: "Health",
                render: (s: Source) => <HealthBadge h={ops.health.get(s.id)} />,
              },
            ]}
            rows={rows}
          />
        ) : (
          <EmptyState
            icon={kind === "replica" ? "copy" : "database"}
            title={
              kind === "replica"
                ? "No replica sources"
                : "No sources match this view"
            }
            description={
              kind === "replica"
                ? "Replica sources are created by a replication job. Set one up under Replication."
                : "Create a source, then attach it to a feed of the same format."
            }
          />
        ))}
      {current && (
        <SourceDrawer
          key={current.id}
          initialTab={fresh === current.id ? "upstream" : "overview"}
          client={client}
          source={current}
          onClose={() => setOpen("")}
          stats={ops.sources.get(current.id)}
          health={ops.health.get(current.id)}
          refresh={() => {
            all.reload();
            ops.reload();
          }}
        />
      )}
      {creating && (
        <CreateSource
          client={client}
          go={go}
          onClose={() => setCreating(false)}
          created={(s) => {
            setCreating(false);
            all.reload();
            setFresh(s.id);
            setOpen(s.id);
          }}
        />
      )}
    </>
  );
}

function CreateSource({
  client,
  go,
  onClose,
  created,
}: {
  client: Client;
  go?: Go;
  onClose: () => void;
  created: (s: Source) => void;
}) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState<Source["kind"]>("local");
  const [format, setFormat] = useState<Source["format"]>("oci");
  const [draft, setDraft] = useState<UpstreamDraft>(emptyDraft);
  const action = useAction();
  const toast = useToast();
  const remote = kind === "remote";
  const current = remote ? draft.name : name;
  const tooLong = new TextEncoder().encode(current.trim()).length > 200;
  const problem = remote ? draftProblem(draft) : undefined;
  return (
    <Modal
      title="Create source"
      description="A source is where content lives. Pick its format and kind."
      onClose={onClose}
      width={remote ? 640 : undefined}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-source"
            type="submit"
            disabled={action.busy || !current.trim() || tooLong}
          >
            {action.busy ? "Creating…" : "Create source"}
          </Button>
        </>
      }
    >
      <form
        id="create-source"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          let made: Source | undefined;
          let partial: Error | undefined;
          const ok = await action.run(async () => {
            if (tooLong)
              throw Error("Source name must be at most 200 UTF-8 bytes.");
            if (remote) {
              if (problem) throw Error(problem);
              const r = await createRemoteSource(client, format, draft);
              made = r.source;
              partial = r.configError;
              return;
            }
            made = (
              await client.call("createSource", {
                body: { name: name.trim(), kind, format },
              })
            ).data;
          });
          if (ok && made) {
            if (partial)
              toast(
                "Source created without its upstream",
                `${made.name}: ${partial.message}`,
              );
            else toast("Source created", made.name);
            created(made);
          }
        }}
      >
        <div className="cols">
          <Select
            label="Format"
            options={formats.map((f) => ({ value: f, label: f }))}
            value={format}
            onChange={val((v) => {
              setFormat(v as Source["format"]);
              setDraft(emptyDraft);
            })}
          />
          <Select
            label="Source kind"
            options={[
              { value: "local", label: "local · store published content" },
              { value: "remote", label: "remote · cache an upstream" },
            ]}
            value={kind}
            onChange={val((v) => setKind(v as Source["kind"]))}
          />
        </div>
        {remote ? (
          <UpstreamFields
            format={format}
            draft={draft}
            setDraft={setDraft}
            nameHint={
              tooLong
                ? undefined
                : "Up to 200 bytes. Pick a preset or edit freely."
            }
          />
        ) : (
          <Input
            label="Source name"
            mono
            required
            maxLength={200}
            value={name}
            onChange={val(setName)}
            error={tooLong ? "At most 200 UTF-8 bytes." : undefined}
            hint="Up to 200 bytes."
          />
        )}
        <p className="muted small">
          {remote
            ? "Pick a preset or a custom upstream; you can change everything afterwards on the source's Upstream tab."
            : "A local source stores what you publish. Attach it to a feed of the same format and point that feed's publish policy at it."}{" "}
          Need a copy of another Kuvryn Ant instance? Replica sources are
          created under{" "}
          {go ? (
            <button
              type="button"
              className="rowlink"
              onClick={() => go("replication")}
            >
              Replication
            </button>
          ) : (
            "Replication"
          )}
          .
        </p>
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

// cacheSummary reads served (stored or fresh) + revalidated (304) against
// downloaded from upstream.
function cacheSummary(s: SourceStats) {
  const served = s.cache_hits + s.cache_revalidations;
  const total = served + s.cache_misses;
  const ratio = total ? ` · ${((served / total) * 100).toFixed(1)}% hit` : "";
  return `${count(s.cache_hits)} served · ${count(s.cache_revalidations)} revalidated · ${count(s.cache_misses)} downloaded${ratio}`;
}

function SourceDrawer({
  client,
  source,
  stats,
  health,
  onClose,
  refresh,
  initialTab = "overview",
}: {
  client: Client;
  source: Source;
  stats?: SourceStats;
  health?: SourceHealth;
  onClose: () => void;
  refresh: () => void;
  initialTab?: string;
}) {
  const remote = source.kind === "remote";
  const [tab, setTab] = useState(remote ? initialTab : "overview");
  return (
    <Drawer
      open
      title={
        <span className="titled">
          <SourceIcon source={source} size="md" />
          {source.name}
        </span>
      }
      description={
        <span className="row">
          <Badge tone="outline">{source.format}</Badge>
          {source.kind}
          {iconForSource(source).upstream && (
            <span className="muted">
              · {iconForSource(source).upstream?.name}
            </span>
          )}
          <HealthBadge h={health} />
        </span>
      }
      onClose={onClose}
      width={600}
      footer={<Button onClick={onClose}>Done</Button>}
    >
      {health?.status === "failing" && (
        <Alert tone="bad" title="Upstream is failing">
          {health.last_status_code ? `HTTP ${health.last_status_code}. ` : ""}
          {health.last_success_at
            ? `Last success ${when(health.last_success_at)}.`
            : "No successful request recorded."}
        </Alert>
      )}
      {remote && (
        <Tabs
          value={tab}
          onChange={setTab}
          items={[
            { id: "overview", label: "Overview" },
            { id: "upstream", label: "Upstream" },
          ]}
        />
      )}
      {tab === "overview" && (
        <>
          <PropertyList
            items={[
              { label: "Source ID", value: <Copyable text={source.id} /> },
              { label: "Revision", value: source.revision, mono: true },
              {
                label: "Attached to",
                value:
                  stats == null
                    ? "—"
                    : `${count(stats.attached_feeds)} feed(s)`,
              },
              { label: "Stored", value: bytes(stats?.bytes) },
              ...(stats
                ? [
                    {
                      label: "Packages",
                      value: `${count(stats.packages)} · ${count(stats.versions)} versions`,
                    },
                    {
                      label: "Cache (all time)",
                      value: cacheSummary(stats),
                    },
                  ]
                : []),
              ...(health && health.status !== "not_applicable"
                ? [
                    {
                      label: "Upstream requests",
                      value: `${count(health.requests)} (${count(health.failures)} failed)`,
                    },
                    ...(health.last_error
                      ? [{ label: "Last error", value: health.last_error }]
                      : []),
                  ]
                : []),
            ]}
          />
          <Rename client={client} source={source} refresh={refresh} />
          <DeleteSource
            client={client}
            source={source}
            attached={stats?.attached_feeds ?? 0}
            done={() => {
              refresh();
              onClose();
            }}
          />
          {remote ? null : source.kind === "local" ? (
            <Alert tone="info" title="Local source">
              Stores content published through feeds whose publish policy
              targets it. Content is immutable once written.
            </Alert>
          ) : (
            <Alert tone="info" title="Replica source">
              Filled by a replication job from another Kuvryn Ant instance and
              read-only here. Manage it under Replication.
            </Alert>
          )}
        </>
      )}
      {remote && tab === "upstream" && (
        <>
          <RemoteSource client={client} source={source} onSaved={refresh} />
          <RemoteOps client={client} source={source} changed={refresh} />
        </>
      )}
    </Drawer>
  );
}

function Rename({
  client,
  source,
  refresh,
}: {
  client: Client;
  source: Source;
  refresh: () => void;
}) {
  const [name, setName] = useState(source.name);
  const current = useResource(
    () => client.call("getSource", { params: { id: source.id } }),
    [client, source.id],
  );
  const action = useAction();
  const toast = useToast();
  const tooLong = new TextEncoder().encode(name.trim()).length > 200;
  return (
    <form
      className="stack"
      onSubmit={async (e) => {
        e.preventDefault();
        const ok = await action.run(async () => {
          await client.call("updateSource", {
            params: { id: source.id },
            etag: requireETag(current.value?.etag ?? null),
            body: { name: name.trim() },
          });
        });
        if (ok) {
          toast("Source renamed", name.trim());
          current.reload();
          refresh();
        }
      }}
    >
      <div className="row end">
        <Input
          label="Source name"
          mono
          required
          maxLength={200}
          value={name}
          onChange={val(setName)}
          error={tooLong ? "At most 200 UTF-8 bytes." : undefined}
        />
        <Button
          type="submit"
          variant="secondary"
          disabled={
            action.busy ||
            !name.trim() ||
            tooLong ||
            name.trim() === source.name ||
            !current.value
          }
        >
          {action.busy ? "Saving…" : "Rename"}
        </Button>
      </div>
      <Failure error={action.error ?? current.error} />
    </form>
  );
}

function RemoteOps({
  client,
  source,
  changed,
}: {
  client: Client;
  source: Source;
  changed: () => void;
}) {
  const [note, setNote] = useState<{ tone: "good" | "bad"; text: string }>();
  const [unavailable, setUnavailable] = useState("");
  const [purge, setPurge] = useState(false);
  const action = useAction();
  const toast = useToast();
  async function call(
    run: () => Promise<{ tone: "good" | "bad"; text: string } | void>,
    label: string,
    done: string,
  ) {
    setUnavailable("");
    setNote(undefined);
    await action.run(async () => {
      try {
        const result = await run();
        setNote(result ?? { tone: "good", text: done });
        toast(label, source.name);
        changed();
      } catch (e) {
        if (isUnavailable(e)) setUnavailable(label);
        else throw e;
      }
    });
  }
  return (
    <SourceOps>
      <div className="row">
        <Button
          size="sm"
          variant="secondary"
          icon="plug-zap"
          disabled={action.busy}
          onClick={() =>
            call(
              async () => {
                const r = (
                  await client.call("testSource", { params: { id: source.id } })
                ).data;
                const ok = r.reachable && r.authenticated;
                return {
                  tone: ok ? "good" : "bad",
                  text: ok
                    ? `Upstream reachable and authenticated (${r.latency_ms} ms).`
                    : `Upstream ${r.reachable ? "reachable but not authenticated" : "unreachable"}${r.status_code ? ` (HTTP ${r.status_code})` : ""}.`,
                };
              },
              "Connection test",
              "Upstream reachable.",
            )
          }
        >
          Test connection
        </Button>
        <Button
          size="sm"
          variant="secondary"
          icon="trash-2"
          disabled={action.busy}
          onClick={() => setPurge(true)}
        >
          Purge cache
        </Button>
      </div>
      {unavailable && <Unavailable what={unavailable} compact />}
      {note && <Alert tone={note.tone} title={note.text} />}
      <Failure error={action.error} />
      {purge && (
        <Modal
          title="Purge cache"
          description={`Cached content for ${source.name} is removed and fetched again on demand. Published content is not affected.`}
          onClose={() => setPurge(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setPurge(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                icon="trash-2"
                onClick={() => {
                  setPurge(false);
                  void call(
                    async () => {
                      await client.call("purgeSourceCache", {
                        params: { id: source.id },
                      });
                    },
                    "Cache purged",
                    "Cache purged.",
                  );
                }}
              >
                Purge cache
              </Button>
            </>
          }
        />
      )}
    </SourceOps>
  );
}
function SourceOps({ children }: { children: React.ReactNode }) {
  return (
    <section className="stack">
      <h4 className="h4">Operations</h4>
      {children}
    </section>
  );
}

function DeleteSource({
  client,
  source,
  attached,
  done,
}: {
  client: Client;
  source: Source;
  attached: number;
  done: () => void;
}) {
  const [open, setOpen] = useState(false);
  const [confirm, setConfirm] = useState("");
  const [force, setForce] = useState(false);
  const current = useResource(
    () => client.call("getSource", { params: { id: source.id } }),
    [client, source.id],
  );
  const action = useAction();
  const toast = useToast();
  return (
    <SourceOps>
      <div className="row">
        <Button
          size="sm"
          variant="secondary"
          icon="trash-2"
          disabled={action.busy || !current.value}
          onClick={() => setOpen(true)}
        >
          Delete source
        </Button>
      </div>
      <Failure error={action.error} />
      {open && (
        <Modal
          title="Delete source"
          description={`Removes ${source.name}. A source that is attached to feeds or holds content is refused unless forced; forcing detaches it and removes its content (instance administrators only).${attached > 0 ? ` It is attached to ${count(attached)} feed(s).` : ""}`}
          onClose={() => setOpen(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setOpen(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                icon="trash-2"
                disabled={confirm !== source.name || action.busy}
                onClick={async () => {
                  const ok = await action.run(async () => {
                    await client.call("deleteSource", {
                      params: { id: source.id },
                      etag: requireETag(current.value?.etag ?? null),
                      query: force ? { force: "true" } : {},
                    });
                  });
                  if (ok) {
                    setOpen(false);
                    toast("Source deleted", source.name);
                    done();
                  }
                }}
              >
                Delete source
              </Button>
            </>
          }
        >
          <Input
            label={`Type ${source.name} to confirm`}
            mono
            value={confirm}
            onChange={val(setConfirm)}
            autoComplete="off"
          />
          <label className="row">
            <input
              type="checkbox"
              checked={force}
              onChange={(e) => setForce(e.target.checked)}
            />
            Force: detach from feeds and remove content
          </label>
        </Modal>
      )}
    </SourceOps>
  );
}
