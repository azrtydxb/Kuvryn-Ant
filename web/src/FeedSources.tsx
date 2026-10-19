import { useState } from "react";
import { requireETag, type Client } from "./api";
import type { Go } from "./App";
import { useAllSources, sourcesForFormat, useOpsMaps } from "./data";
import { isUnavailable } from "./gaps";
import { HealthBadge } from "./Health";
import type {
  Feed,
  ResolveExplanation,
  SourceAttachmentPage,
} from "./generated";
import { RouteCandidates } from "./FederationRouting";
import {
  Alert,
  Badge,
  Button,
  DropdownMenu,
  Icon,
  IconButton,
  Input,
  Modal,
  Select,
} from "./ds/bundle";
import {
  Card,
  Failure,
  Loading,
  Mono,
  Pager,
  Unavailable,
  useAction,
  useResource,
  useToast,
  val,
} from "./ui";

type Attachment = SourceAttachmentPage["items"][number];

export function FeedSources({
  client,
  feed,
  go,
  refresh,
}: {
  client: Client;
  feed: Feed;
  go: Go;
  refresh: () => void;
}) {
  const [cursor, setCursor] = useState("");
  const r = useResource(
    () =>
      client.call("listFeedSources", {
        params: { id: feed.id },
        query: { limit: "50", ...(cursor ? { cursor } : {}) },
      }),
    [client, feed.id, cursor],
  );
  const ops = useOpsMaps(client);
  const [order, setOrder] = useState<string[]>();
  const [attaching, setAttaching] = useState(false);
  const [priorityFor, setPriorityFor] = useState<Attachment>();
  const [detaching, setDetaching] = useState<Attachment>();
  const [unavailable, setUnavailable] = useState(false);
  const save = useAction();
  const toast = useToast();
  const items = r.value?.data.items ?? [];
  const shown = order
    ? (order
        .map((id) => items.find((a) => a.source.id === id))
        .filter(Boolean) as Attachment[])
    : items;
  const dirty = !!order;
  const move = (i: number, d: number) => {
    const n = shown.map((a) => a.source.id);
    [n[i], n[i + d]] = [n[i + d], n[i]];
    setOrder(n);
  };
  const etag = () => requireETag(r.value?.etag ?? null);
  return (
    <div className="split">
      <Card
        flush
        className="grow2"
        title="Sources in read order"
        actions={
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            onClick={() => setAttaching(true)}
          >
            Attach source
          </Button>
        }
      >
        <p className="card__note">
          Clients get the first source that has the package; later sources are
          shadowed for that name. Lower priorities are read first. Only sources
          you can read are shown, so this may not be the complete order.
        </p>
        <Failure error={r.error} className="pad" />
        {r.loading && <Loading what="attachments" />}
        {r.value && !items.length && (
          <p className="pad muted">No visible attached sources.</p>
        )}
        <ol className="srclist">
          {shown.map((a, i) => {
            return (
              <li key={a.source.id} className="srcrow">
                <Icon name="grip-vertical" size={16} className="muted" />
                <span className="pos">{i + 1}</span>
                <div className="srcrow__main">
                  <div className="row tight">
                    <Mono>{a.source.name}</Mono>
                    <Badge tone="neutral">{a.source.kind}</Badge>
                    <Badge tone="outline">{a.source.format}</Badge>
                  </div>
                  <div className="muted small">
                    Priority <Mono>{a.priority}</Mono>
                  </div>
                </div>
                <HealthBadge h={ops.health.get(a.source.id)} />
                <IconButton
                  icon="arrow-up"
                  label={`Move ${a.source.name} up`}
                  disabled={i === 0}
                  onClick={() => move(i, -1)}
                />
                <IconButton
                  icon="arrow-down"
                  label={`Move ${a.source.name} down`}
                  disabled={i === shown.length - 1}
                  onClick={() => move(i, 1)}
                />
                <DropdownMenu
                  align="end"
                  trigger={
                    <IconButton
                      icon="ellipsis"
                      label={`Actions for ${a.source.name}`}
                    />
                  }
                  items={[
                    {
                      label: "Set priority",
                      icon: "list-ordered",
                      onSelect: () => setPriorityFor(a),
                    },
                    {
                      label: "Open source",
                      icon: "external-link",
                      onSelect: () => go("sources", { source: a.source.id }),
                    },
                    { divider: true },
                    {
                      label: "Detach",
                      icon: "unlink",
                      danger: true,
                      onSelect: () => setDetaching(a),
                    },
                  ]}
                />
              </li>
            );
          })}
        </ol>
        {dirty && (
          <div className="savebar">
            <span>Unsaved order</span>
            <div className="row">
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  setOrder(undefined);
                  setUnavailable(false);
                }}
              >
                Discard
              </Button>
              <Button
                size="sm"
                disabled={save.busy}
                onClick={async () => {
                  setUnavailable(false);
                  let gone = false;
                  const ok = await save.run(async () => {
                    try {
                      await client.call("reorderFeedSources", {
                        params: { id: feed.id },
                        etag: etag(),
                        body: { source_ids: order! },
                      });
                    } catch (e) {
                      if (!isUnavailable(e)) throw e;
                      gone = true;
                      setUnavailable(true);
                    }
                  });
                  if (ok && !gone) {
                    toast("Read order saved", feed.slug);
                    setOrder(undefined);
                    refresh();
                  }
                }}
              >
                {save.busy ? "Saving…" : "Save order"}
              </Button>
            </div>
          </div>
        )}
        {unavailable && (
          <Alert
            tone="info"
            title="Atomic reorder: Not available on this server yet"
            className="pad-alert"
          >
            Use Set priority on a row to move a source to an unoccupied priority
            instead.
          </Alert>
        )}
        <Failure error={save.error} className="pad-alert" />
        {r.value && (
          <div className="pad">
            <Pager
              cursor={cursor}
              next={r.value.data.page.next_cursor}
              set={setCursor}
            />
          </div>
        )}
      </Card>
      <Resolve client={client} feed={feed} />
      {attaching && r.value && (
        <AttachSource
          client={client}
          feed={feed}
          attached={items.map((a) => a.source.id)}
          nextPriority={items.reduce((m, a) => Math.max(m, a.priority), 0) + 10}
          etag={etag}
          onClose={() => setAttaching(false)}
          done={() => {
            setAttaching(false);
            refresh();
          }}
        />
      )}
      {priorityFor && r.value && (
        <SetPriority
          client={client}
          feed={feed}
          item={priorityFor}
          etag={etag}
          onClose={() => setPriorityFor(undefined)}
          done={() => {
            setPriorityFor(undefined);
            refresh();
          }}
        />
      )}
      {detaching && r.value && (
        <Detach
          client={client}
          feed={feed}
          item={detaching}
          etag={etag}
          onClose={() => setDetaching(undefined)}
          done={() => {
            setDetaching(undefined);
            refresh();
          }}
        />
      )}
    </div>
  );
}

function AttachSource({
  client,
  feed,
  attached,
  nextPriority,
  etag,
  onClose,
  done,
}: {
  client: Client;
  feed: Feed;
  attached: string[];
  nextPriority: number;
  etag: () => string;
  onClose: () => void;
  done: () => void;
}) {
  const sources = useAllSources(client);
  const candidates = sourcesForFormat(sources.items, feed.format).filter(
    (s) => !attached.includes(s.id),
  );
  const [source, setSource] = useState("");
  const [priority, setPriority] = useState(String(nextPriority));
  const action = useAction();
  const toast = useToast();
  const chosen = source || candidates[0]?.id || "";
  const n = Number(priority);
  const valid =
    Number.isInteger(n) && n >= 0 && n <= 1000000 && priority !== "";
  return (
    <Modal
      title="Attach source"
      description={`Only ${feed.format} sources can be attached to this feed.`}
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!chosen || !valid || action.busy}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("attachFeedSource", {
                  params: { id: feed.id, source_id: chosen },
                  etag: etag(),
                  body: { priority: n },
                }),
              );
              if (ok) {
                toast("Source attached", `Priority ${n}`);
                done();
              }
            }}
          >
            {action.busy ? "Attaching…" : "Attach source"}
          </Button>
        </>
      }
    >
      {sources.loading && <Loading what="sources" />}
      <Failure error={sources.error} />
      {!sources.loading && !candidates.length ? (
        <p className="muted">
          No unattached {feed.format} sources are available to you. Create one
          under Sources first.
        </p>
      ) : (
        <Select
          label="Source"
          options={candidates.map((s) => ({
            value: s.id,
            label: `${s.name} (${s.kind})`,
          }))}
          value={chosen}
          onChange={val(setSource)}
        />
      )}
      <Input
        label="Read priority"
        type="number"
        mono
        min={0}
        max={1000000}
        required
        value={priority}
        onChange={val(setPriority)}
        hint="Lower is read first. Use an unoccupied priority between 0 and 1,000,000."
      />
      <Failure error={action.error} />
    </Modal>
  );
}

function SetPriority({
  client,
  feed,
  item,
  etag,
  onClose,
  done,
}: {
  client: Client;
  feed: Feed;
  item: Attachment;
  etag: () => string;
  onClose: () => void;
  done: () => void;
}) {
  const [priority, setPriority] = useState(String(item.priority));
  const action = useAction();
  const toast = useToast();
  const n = Number(priority);
  const valid =
    Number.isInteger(n) && n >= 0 && n <= 1000000 && priority !== "";
  return (
    <Modal
      title={`Priority for ${item.source.name}`}
      description="Set an unoccupied priority to move this source. There is no atomic swap; the server rejects conflicts."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!valid || action.busy}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("attachFeedSource", {
                  params: { id: feed.id, source_id: item.source.id },
                  etag: etag(),
                  body: { priority: n },
                }),
              );
              if (ok) {
                toast("Priority updated", `${item.source.name} → ${n}`);
                done();
              }
            }}
          >
            {action.busy ? "Saving…" : "Update priority"}
          </Button>
        </>
      }
    >
      <Input
        label={`Read priority for ${item.source.name}`}
        type="number"
        mono
        min={0}
        max={1000000}
        required
        value={priority}
        onChange={val(setPriority)}
      />
      <Failure error={action.error} />
    </Modal>
  );
}

function Detach({
  client,
  feed,
  item,
  etag,
  onClose,
  done,
}: {
  client: Client;
  feed: Feed;
  item: Attachment;
  etag: () => string;
  onClose: () => void;
  done: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Detach source"
      description={`${item.source.name} stops serving this feed. The source and its content are kept.`}
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            icon="unlink"
            disabled={action.busy}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("detachFeedSource", {
                  params: { id: feed.id, source_id: item.source.id },
                  etag: etag(),
                }),
              );
              if (ok) {
                toast("Source detached", item.source.name);
                done();
              }
            }}
          >
            {action.busy ? "Detaching…" : "Detach"}
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
    </Modal>
  );
}

function Resolve({ client, feed }: { client: Client; feed: Feed }) {
  const [coordinate, setCoordinate] = useState("");
  const [result, setResult] = useState<ResolveExplanation>();
  const [unavailable, setUnavailable] = useState(false);
  const action = useAction();
  return (
    <Card title="Resolve a package" className="grow1">
      <p className="muted small">See which source would serve a request.</p>
      <form
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          setResult(undefined);
          setUnavailable(false);
          await action.run(async () => {
            try {
              const res = await client.call("resolveFeedCoordinate", {
                params: { id: feed.id },
                query: { coordinate },
              });
              setResult(res.data);
            } catch (err) {
              if (isUnavailable(err)) setUnavailable(true);
              else throw err;
            }
          });
        }}
      >
        <Input
          size="sm"
          mono
          label="Package coordinate"
          placeholder="library/postgres:17.2-alpine"
          required
          maxLength={512}
          value={coordinate}
          onChange={val(setCoordinate)}
        />
        <Button
          type="submit"
          size="sm"
          variant="secondary"
          disabled={action.busy}
        >
          {action.busy ? "Resolving…" : "Resolve"}
        </Button>
      </form>
      {unavailable && <Unavailable what="Resolution explain" compact />}
      <Failure error={action.error} />
      {result && (
        <Alert
          tone={result.status === "not_found" ? "warn" : "info"}
          title={
            result.serving
              ? `Served by ${result.serving.name}`
              : result.status === "federated"
                ? "Not here: would be routed to another federation member"
                : result.status === "pull_through"
                  ? "Would be pulled through an upstream"
                  : result.status === "namespace_mismatch"
                    ? "Namespace does not match"
                    : "Not found in any visible source"
          }
        >
          {result.sources
            .filter((x) => x.role !== "serving")
            .map((x) => `${x.name}: ${x.role.replace("_", " ")} (${x.reason})`)
            .join("; ")}
          {result.hidden_sources > 0 &&
            ` ${result.hidden_sources} source(s) you cannot read also take part.`}
          {result.cache.state !== "not_applicable" &&
            ` Cache: ${result.cache.state.replace("_", " ")}.`}
        </Alert>
      )}
      {result?.federation && <RouteCandidates plan={result.federation} />}
    </Card>
  );
}
