import { useEffect, useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllSources } from "./data";
import { isUnavailable, paths, type SourceStats } from "./gaps";
import { formats, type Source } from "./generated";
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
import {
  Copyable,
  Failure,
  Loading,
  Needs,
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

const healthTone = {
  healthy: "good",
  degraded: "warn",
  failing: "bad",
} as const;
function Health({ s }: { s: Source }) {
  const h = (s as SourceStats & Source).health;
  if (!h?.state) return <span className="muted">—</span>;
  return (
    <Badge tone={healthTone[h.state] ?? "neutral"} dot>
      {h.state === "healthy"
        ? "Healthy"
        : (h.message ?? (h.state === "degraded" ? "Degraded" : "Failing"))}
      {h.p95_ms != null && h.state !== "healthy" && ` · ${h.p95_ms} ms p95`}
    </Badge>
  );
}

export function SourcesPage({
  client,
  openId,
}: {
  client: Client;
  openId: string;
}) {
  const all = useAllSources(client);
  const [kind, setKind] = useState("all");
  const [filter, setFilter] = useState("");
  const [open, setOpen] = useState<string>(openId);
  const [creating, setCreating] = useState(false);
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
        text="Where content lives: local storage, cached upstream registries, or replicas. Attach sources to feeds of the same format."
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
                    {s.name}
                    <small>
                      {(s as SourceStats & Source).origin ??
                        (s.kind === "remote"
                          ? "Remote upstream"
                          : s.kind === "local"
                            ? "Local storage"
                            : "Replica")}
                    </small>
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
                  const n = (s as SourceStats & Source).feed_count;
                  return n == null ? "—" : `${n} ${n === 1 ? "feed" : "feeds"}`;
                },
              },
              {
                key: "size",
                label: "Stored",
                align: "right",
                render: (s: Source) =>
                  bytes((s as SourceStats & Source).size_bytes),
              },
              {
                key: "health",
                label: "Health",
                render: (s: Source) => <Health s={s} />,
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
                ? "Replication setup is not available in this console."
                : "Create a source, then attach it to a feed of the same format."
            }
          />
        ))}
      <Needs block>source health, size, usage counts</Needs>
      {current && (
        <SourceDrawer
          key={current.id}
          client={client}
          source={current}
          onClose={() => setOpen("")}
          refresh={all.reload}
        />
      )}
      {creating && (
        <CreateSource
          client={client}
          onClose={() => setCreating(false)}
          created={(s) => {
            setCreating(false);
            all.reload();
            setOpen(s.id);
          }}
        />
      )}
    </>
  );
}

function CreateSource({
  client,
  onClose,
  created,
}: {
  client: Client;
  onClose: () => void;
  created: (s: Source) => void;
}) {
  const [name, setName] = useState("");
  const [kind, setKind] = useState<Source["kind"]>("local");
  const [format, setFormat] = useState<Source["format"]>("oci");
  const action = useAction();
  const toast = useToast();
  const tooLong = new TextEncoder().encode(name.trim()).length > 200;
  return (
    <Modal
      title="Create source"
      description="Remote trust, cache and credentials are configured right after creation."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-source"
            type="submit"
            disabled={action.busy || !name.trim() || tooLong}
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
          const ok = await action.run(async () => {
            if (tooLong)
              throw Error("Source name must be at most 200 UTF-8 bytes.");
            made = (
              await client.call("createSource", {
                body: { name: name.trim(), kind, format },
              })
            ).data;
          });
          if (ok && made) {
            toast("Source created", made.name);
            created(made);
          }
        }}
      >
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
        <div className="cols">
          <Select
            label="Format"
            options={formats.map((f) => ({ value: f, label: f }))}
            value={format}
            onChange={val((v) => setFormat(v as Source["format"]))}
          />
          <Select
            label="Source kind"
            options={[
              { value: "local", label: "local · store published content" },
              { value: "remote", label: "remote · cache an upstream" },
              { value: "replica", label: "replica · setup not available here" },
            ]}
            value={kind}
            onChange={val((v) => setKind(v as Source["kind"]))}
          />
        </div>
        <p className="muted small">
          This creates the source record only. Source kind and format are
          configuration; the API does not report which protocols are served.
        </p>
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function SourceDrawer({
  client,
  source,
  onClose,
  refresh,
}: {
  client: Client;
  source: Source;
  onClose: () => void;
  refresh: () => void;
}) {
  const remote = source.kind === "remote";
  const stats = source as SourceStats & Source;
  return (
    <Drawer
      open
      title={source.name}
      description={
        <span className="row">
          <Badge tone="outline">{source.format}</Badge>
          {source.kind}
          <Health s={source} />
        </span>
      }
      onClose={onClose}
      width={600}
      footer={<Button onClick={onClose}>Done</Button>}
    >
      {stats.health?.state === "failing" && (
        <Alert tone="bad" title={stats.health.message ?? "Upstream is failing"}>
          {stats.health.last_success_at &&
            `Last success ${when(stats.health.last_success_at)}.`}
        </Alert>
      )}
      <PropertyList
        items={[
          { label: "Source ID", value: <Copyable text={source.id} /> },
          { label: "Revision", value: source.revision, mono: true },
          {
            label: "Attached to",
            value:
              stats.feed_count == null
                ? "—"
                : `${count(stats.feed_count)} feed(s)`,
          },
          { label: "Stored", value: bytes(stats.size_bytes) },
        ]}
      />
      <Rename client={client} source={source} refresh={refresh} />
      {remote ? (
        <>
          <RemoteSource client={client} source={source} />
          <RemoteOps client={client} source={source} />
        </>
      ) : source.kind === "local" ? (
        <Alert tone="info" title="Local source">
          Stores content published through feeds whose publish policy targets
          it. Content is immutable once written.
        </Alert>
      ) : (
        <Alert tone="info" title="Replica source">
          Replication setup is not available in this console.
        </Alert>
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

function RemoteOps({ client, source }: { client: Client; source: Source }) {
  const [note, setNote] = useState<{ tone: "good" | "info"; text: string }>();
  const [unavailable, setUnavailable] = useState("");
  const [purge, setPurge] = useState(false);
  const action = useAction();
  const toast = useToast();
  async function call(
    run: () => Promise<unknown>,
    label: string,
    done: string,
  ) {
    setUnavailable("");
    setNote(undefined);
    await action.run(async () => {
      try {
        await run();
        setNote({ tone: "good", text: done });
        toast(label, source.name);
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
              () =>
                client.extra("POST", paths.sourceTest(source.id), { body: {} }),
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
      <Needs>connection test</Needs>
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
                    () =>
                      client.call("purgeSourceCache", {
                        params: { id: source.id },
                      }),
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
