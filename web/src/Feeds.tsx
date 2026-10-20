import { FeedAccess } from "./ClientAuth";
import { useEffect, useState } from "react";
import { requireETag, type Client } from "./api";
import type { Go } from "./App";
import { useAllSources, sourcesForFormat, useOpsMaps } from "./data";
import { isUnavailable } from "./gaps";
import { formats, type Feed, type Source } from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  CodeBlock,
  DropdownMenu,
  EmptyState,
  Input,
  Modal,
  PropertyList,
  SettingsRow,
  SettingsSection,
  Select,
  Stepper,
  Switch,
  Table,
  Tabs,
} from "./ds/bundle";
import { FeedSources } from "./FeedSources";
import {
  CacheBadge,
  CachePolicyFields,
  cacheFormats,
  draftToInput,
  isCache,
  policySummary,
  useDraft,
} from "./CachePolicy";
import { PolicyTab } from "./Policy";
import { FeedSourceIcons, SourceIcon } from "./SourceIcon";
import {
  UpstreamFields,
  createRemoteSource,
  draftProblem,
  emptyDraft,
  type UpstreamDraft,
} from "./UpstreamFields";
import { FeedPackages, pullCommand, type PackageRef } from "./Packages";
import {
  Card,
  Copyable,
  Failure,
  Loading,
  PageHeading,
  Pager,
  Toolbar,
  Unavailable,
  bytes,
  chk,
  count,
  useAction,
  useDebounced,
  useResource,
  useToast,
  val,
} from "./ui";

const slugPattern = /^[a-z0-9][a-z0-9_-]{0,62}$/;
export const endpoint = (slug: string) =>
  `${location.protocol}//${location.host}/feeds/${slug}`;

export function FeedsPage({
  client,
  feedId,
  create,
  go,
  setCrumb,
  openPackage,
  closeCreate,
  startCreate,
}: {
  client: Client;
  feedId: string;
  create: boolean;
  go: Go;
  setCrumb: (s: string) => void;
  openPackage: (p: PackageRef) => void;
  closeCreate: () => void;
  startCreate: () => void;
}) {
  const [epoch, setEpoch] = useState(0);
  if (feedId)
    return (
      <FeedDetail
        client={client}
        id={feedId}
        back={() => go("feeds")}
        setCrumb={setCrumb}
        openPackage={openPackage}
        go={go}
      />
    );
  return (
    <>
      <FeedList
        client={client}
        epoch={epoch}
        open={(id) => go("feeds", { feed: id })}
        startCreate={startCreate}
      />
      {create && (
        <CreateFeed
          client={client}
          onClose={closeCreate}
          created={(id) => {
            closeCreate();
            setEpoch((v) => v + 1);
            go("feeds", { feed: id });
          }}
          refresh={() => setEpoch((v) => v + 1)}
        />
      )}
    </>
  );
}

function FeedList({
  client,
  epoch,
  open,
  startCreate,
}: {
  client: Client;
  epoch: number;
  open: (id: string) => void;
  startCreate: () => void;
}) {
  const [cursor, setCursor] = useState("");
  const [filter, setFilter] = useState("");
  const [purpose, setPurpose] = useState("");
  const prefix = useDebounced(filter);
  const r = useResource(
    () =>
      client.call("listFeeds", {
        query: {
          limit: "50",
          ...(cursor ? { cursor } : {}),
          prefix,
          ...(purpose ? { purpose } : {}),
        },
      }),
    [client, cursor, prefix, purpose, epoch],
  );
  const ops = useOpsMaps(client);
  const allSources = useAllSources(client);
  const sourceById = new Map(allSources.items.map((s) => [s.id, s]));
  const rows = r.value?.data.items ?? [];
  // Remote upstreams (and the cache mark) of a feed, from the one stats call.
  const feedIcons = (f: Feed) => {
    const attached = (ops.feeds.get(f.id)?.source_ids ?? [])
      .map((id) => sourceById.get(id))
      .filter((s) => !!s);
    return attached.some((s) => s.kind === "remote") || isCache(f) ? (
      <FeedSourceIcons sources={attached} cache={isCache(f)} />
    ) : null;
  };
  return (
    <>
      <PageHeading
        eyebrow="Repositories"
        title="Feeds"
        text="Stable endpoints clients pull from and publish to. Each feed reads from an ordered list of sources."
      >
        <Button icon="plus" onClick={startCreate}>
          Create feed
        </Button>
      </PageHeading>
      <Toolbar
        right={
          r.value
            ? `${rows.length} feed${rows.length === 1 ? "" : "s"} on this page`
            : undefined
        }
      >
        <Input
          icon="search"
          size="sm"
          aria-label="Filter by slug prefix"
          placeholder="Filter by slug prefix"
          maxLength={200}
          value={filter}
          onChange={(e: { target: { value: string } }) => {
            setFilter(e.target.value);
            setCursor("");
          }}
          className="w260"
        />
        <Select
          size="sm"
          aria-label="Filter by purpose"
          value={purpose}
          options={[
            { value: "", label: "All purposes" },
            { value: "artifacts", label: "Artifacts" },
            { value: "cache", label: "Build caches" },
          ]}
          onChange={(e: { target: { value: string } }) => {
            setPurpose(e.target.value);
            setCursor("");
          }}
        />
        <Button
          size="sm"
          variant="secondary"
          icon="refresh-cw"
          disabled={r.loading}
          onClick={r.reload}
        >
          Refresh
        </Button>
      </Toolbar>
      <Failure error={r.error} />
      {r.loading && <Loading what="feeds" />}
      {r.value &&
        (rows.length ? (
          <>
            <Table
              onRowClick={(f: Feed) => open(f.id)}
              columns={[
                {
                  key: "slug",
                  label: "Feed",
                  render: (f: Feed) => (
                    <button
                      type="button"
                      className="rowlink"
                      onClick={(e) => {
                        e.stopPropagation();
                        open(f.id);
                      }}
                    >
                      {f.slug}
                      <small>{endpoint(f.slug)}</small>
                      <span className="narrow-only">{feedIcons(f)}</span>
                    </button>
                  ),
                },
                {
                  key: "format",
                  label: "Format",
                  render: (f: Feed) => (
                    <>
                      <Badge tone="outline">{f.format}</Badge>
                      {isCache(f) && (
                        <>
                          {" "}
                          <CacheBadge />
                        </>
                      )}
                    </>
                  ),
                },
                {
                  key: "upstreams",
                  label: "Upstreams",
                  render: (f: Feed) =>
                    feedIcons(f) ?? <span className="muted">—</span>,
                },
                {
                  key: "anonymous_read",
                  label: "Access",
                  render: (f: Feed) =>
                    f.anonymous_read ? (
                      <Badge tone="warn" icon="globe">
                        Anonymous read
                      </Badge>
                    ) : (
                      <Badge tone="neutral" icon="lock">
                        Authenticated
                      </Badge>
                    ),
                },
                {
                  key: "packages",
                  label: "Packages",
                  align: "right",
                  render: (f: Feed) => count(ops.feeds.get(f.id)?.packages),
                },
                {
                  key: "size",
                  label: "Stored",
                  align: "right",
                  render: (f: Feed) => bytes(ops.feeds.get(f.id)?.bytes),
                },
                { key: "revision", label: "Revision", align: "right" },
              ]}
              rows={rows}
            />
            <Pager
              cursor={cursor}
              next={r.value.data.page.next_cursor}
              set={setCursor}
            />
          </>
        ) : (
          <EmptyState
            icon="rss"
            title="No feeds match this view"
            description="Create a feed to get a stable endpoint for clients."
            action={
              <Button size="sm" icon="plus" onClick={startCreate}>
                Create feed
              </Button>
            }
          />
        ))}
    </>
  );
}

function CreateFeed({
  client,
  onClose,
  created,
  refresh,
}: {
  client: Client;
  onClose: () => void;
  created: (id: string) => void;
  refresh: () => void;
}) {
  const [step, setStep] = useState(0);
  const [slug, setSlug] = useState("");
  const [format, setFormat] = useState<Feed["format"]>("oci");
  const [purpose, setPurpose] = useState<"artifacts" | "cache">("artifacts");
  const [draft, setDraft] = useDraft();
  const [anon, setAnon] = useState(false);
  const [picked, setPicked] = useState<string[]>([]);
  const [target, setTarget] = useState("");
  const [made, setMade] = useState("");
  const [adding, setAdding] = useState(false);
  const [draft2, setDraft2] = useState<UpstreamDraft>(emptyDraft);
  const toast = useToast();
  const sources = useAllSources(client);
  const action = useAction();
  const addUpstream = useAction();
  const cacheInput = purpose === "cache" ? draftToInput(draft) : undefined;
  const valid =
    slugPattern.test(slug) && (purpose !== "cache" || cacheInput !== undefined);
  const matching = sourcesForFormat(sources.items, format);
  const pickedSources = picked
    .map((id) => matching.find((s) => s.id === id))
    .filter((s) => !!s);
  const locals = pickedSources.filter((s) => s.kind === "local");
  const writeTarget =
    target && locals.some((s) => s.id === target)
      ? target
      : (locals[0]?.id ?? "");
  const steps = ["Basics", "Sources", "Review"];
  const upstreamProblem = draftProblem(draft2);
  const upstreamName = draft2.name.trim();
  // Fallback for servers without the batch endpoint: create the feed, then
  // attach sources in read order and set the default publish target. A failure
  // after the feed exists reports the step and offers to open the feed.
  async function submit() {
    let id = made;
    let stage = "create the feed";
    const ok = await action.run(async () => {
      try {
        let revision = 0;
        if (!id) {
          // One idempotent call: feed, read order and publish target.
          try {
            const batch = await client.call("createFeedBatch", {
              body: {
                slug,
                format,
                anonymous_read: anon,
                ...(purpose === "cache"
                  ? { purpose, cache_policy: cacheInput }
                  : {}),
                ...(picked.length ? { source_ids: picked } : {}),
                ...(writeTarget
                  ? {
                      publish_policy: {
                        default_source_id: writeTarget,
                        rules: [],
                      },
                    }
                  : {}),
              },
            });
            id = batch.data.feed.id;
            setMade(id);
            return;
          } catch (e) {
            // Servers without the batch endpoint use the step-by-step flow.
            if (!isUnavailable(e)) throw e;
          }
        }
        if (!id) {
          const feed = await client.call("createFeed", {
            body: {
              slug,
              format,
              anonymous_read: anon,
              ...(purpose === "cache"
                ? { purpose, cache_policy: cacheInput }
                : {}),
            },
          });
          id = feed.data.id;
          revision = feed.data.revision;
          setMade(id);
        } else {
          revision = (await client.call("getFeed", { params: { id } })).data
            .revision;
        }
        stage = "attach sources";
        if (id && picked.length) {
          const current = await client.call("listFeedSources", {
            params: { id },
            query: { limit: "200" },
          });
          const have = new Set(current.data.items.map((a) => a.source.id));
          revision = current.data.feed_revision;
          for (let i = 0; i < picked.length; i++) {
            if (have.has(picked[i])) continue;
            const res = await client.call("attachFeedSource", {
              params: { id, source_id: picked[i] },
              etag: requireETag(`"${revision}"`),
              body: { priority: (i + 1) * 10 },
            });
            revision = res.data.feed_revision;
          }
        }
        stage = "set the publish target";
        if (id && writeTarget) {
          const policy = await client.call("getPublishPolicy", {
            params: { id },
          });
          await client.call("setPublishPolicy", {
            params: { id },
            etag: requireETag(policy.etag),
            body: { default_source_id: writeTarget, rules: [] },
          });
        }
      } catch (e) {
        if (id && stage !== "create the feed") {
          refresh();
          throw Error(
            `The feed was created, but the console could not ${stage}. ` +
              (e instanceof Error ? e.message : "") +
              " Retry, or open the feed to finish setup.",
          );
        }
        throw e;
      }
    });
    if (ok && id) {
      toast("Feed created", slug);
      created(id);
    }
  }
  return (
    <Modal
      title="Create feed"
      description="The feed is created at the last step; sources and the publish target are applied right after."
      onClose={onClose}
      width={620}
      actions={
        <>
          <Button
            variant="secondary"
            disabled={action.busy}
            onClick={step ? () => setStep(step - 1) : onClose}
          >
            {step ? "Back" : "Cancel"}
          </Button>
          {made && (
            <Button variant="ghost" onClick={() => created(made)}>
              Open feed
            </Button>
          )}
          {step < 2 ? (
            <Button
              iconRight="arrow-right"
              disabled={!valid}
              onClick={() => setStep(step + 1)}
            >
              Continue
            </Button>
          ) : (
            <Button icon="check" disabled={action.busy} onClick={submit}>
              {action.busy ? "Creating…" : made ? "Retry setup" : "Create feed"}
            </Button>
          )}
        </>
      }
    >
      <Stepper steps={steps} current={step} />
      {step === 0 && (
        <>
          <Input
            label="Feed slug"
            mono
            required
            maxLength={63}
            value={slug}
            onChange={val(setSlug)}
            error={
              !slug || valid
                ? undefined
                : "Lowercase letters, digits, _ or -; start with a letter or digit; max 63."
            }
            hint={
              valid
                ? "Endpoint: " + endpoint(slug)
                : "Lowercase letters, digits, underscores or hyphens; up to 63 characters."
            }
          />
          <Select
            label="Purpose"
            options={[
              { value: "artifacts", label: "Artifacts (published content)" },
              { value: "cache", label: "Build cache (buildx, sccache)" },
            ]}
            value={purpose}
            onChange={(e: { target: { value: string } }) => {
              const next = e.target.value as "artifacts" | "cache";
              setPurpose(next);
              if (next === "cache" && !cacheFormats.includes(format)) {
                setFormat("oci");
                setPicked([]);
              }
            }}
            hint="A cache feed is evictable, never replicated and hidden from package search. Only oci and raw feeds can be caches."
          />
          {purpose === "cache" && (
            <CachePolicyFields draft={draft} set={setDraft} />
          )}
          <Select
            label="Format"
            options={formats
              .filter((f) => purpose !== "cache" || cacheFormats.includes(f))
              .map((f) => ({ value: f, label: f }))}
            value={format}
            onChange={(e: { target: { value: string } }) => {
              setFormat(e.target.value as Feed["format"]);
              setPicked([]);
              setDraft2(emptyDraft);
            }}
            hint="Cannot be changed after creation."
          />
          <Alert tone="info" title="Protocol support is not reported">
            The API does not report which protocols this deployment serves. A
            configured feed does not establish working publish, pull-through or
            replica support.
          </Alert>
          <SettingsRow
            label="Anonymous reads"
            description="Anyone who can reach the endpoint can pull. Off by default."
          >
            <Switch
              aria-label="Allow anonymous reads"
              checked={anon}
              onChange={chk(setAnon)}
            />
          </SettingsRow>
        </>
      )}
      {step === 1 && (
        <>
          <p className="muted small">
            Pick {format} sources in read order. The first match wins; you can
            reorder later.
          </p>
          {sources.loading && <Loading what="sources" />}
          <Failure error={sources.error} />
          {!sources.loading &&
            (matching.length ? (
              matching.map((s) => (
                <div key={s.id} className="pick">
                  <Checkbox
                    label={s.name}
                    hint={s.kind}
                    checked={picked.includes(s.id)}
                    onChange={() =>
                      setPicked(
                        picked.includes(s.id)
                          ? picked.filter((n) => n !== s.id)
                          : [...picked, s.id],
                      )
                    }
                  />
                  <SourceIcon source={s} size="sm" />
                  {picked.includes(s.id) && (
                    <Badge tone="pillar">#{picked.indexOf(s.id) + 1}</Badge>
                  )}
                </div>
              ))
            ) : (
              <EmptyState
                icon="database"
                title={`No ${format} sources`}
                description="Create a source first, or skip and attach one later."
              />
            ))}
          {adding ? (
            <section className="stack" aria-label="New upstream">
              <h4 className="h4">New upstream</h4>
              <UpstreamFields
                format={format}
                draft={draft2}
                setDraft={setDraft2}
              />
              <Failure error={addUpstream.error} />
              <div className="row">
                <Button
                  variant="secondary"
                  size="sm"
                  disabled={addUpstream.busy}
                  onClick={() => setAdding(false)}
                >
                  Cancel
                </Button>
                <Button
                  size="sm"
                  icon="plus"
                  disabled={
                    addUpstream.busy || !upstreamName || !!upstreamProblem
                  }
                  onClick={async () => {
                    let made2: Source | undefined;
                    const ok = await addUpstream.run(async () => {
                      const res = await createRemoteSource(
                        client,
                        format,
                        draft2,
                      );
                      made2 = res.source;
                      if (res.configError) {
                        sources.reload();
                        setPicked((p) => [...p, res.source.id]);
                        throw Error(
                          "The source was created and selected, but its upstream was not saved: " +
                            res.configError.message +
                            " Finish it on the source's Upstream tab.",
                        );
                      }
                    });
                    if (ok && made2) {
                      toast("Upstream added", made2.name);
                      sources.reload();
                      setPicked((p) => [...p, made2!.id]);
                      setDraft2(emptyDraft);
                      setAdding(false);
                    }
                  }}
                >
                  {addUpstream.busy ? "Creating…" : "Create and select"}
                </Button>
              </div>
            </section>
          ) : (
            <div>
              <Button
                size="sm"
                variant="secondary"
                icon="plus"
                onClick={() => setAdding(true)}
              >
                Add upstream
              </Button>
            </div>
          )}
          {locals.length > 0 && (
            <Select
              label="Default publish target"
              options={[
                ...locals.map((s) => ({ value: s.id, label: s.name })),
                { value: "", label: "Reject unmatched writes" },
              ]}
              value={writeTarget}
              onChange={val(setTarget)}
              hint="Only attached local sources of this format. Rules can be added afterwards."
            />
          )}
        </>
      )}
      {step === 2 && (
        <>
          <PropertyList
            items={[
              { label: "Slug", value: slug, mono: true },
              { label: "Format", value: format },
              {
                label: "Purpose",
                value:
                  purpose === "cache"
                    ? `Build cache · ${policySummary({ max_idle_seconds: cacheInput?.max_idle_seconds ?? 0, max_bytes: cacheInput?.max_bytes || undefined })}`
                    : "Artifacts",
              },
              {
                label: "Access",
                value: anon ? "Anonymous read" : "Authenticated",
              },
              {
                label: "Read order",
                value: pickedSources.length ? (
                  <ol className="reviewsrc">
                    {pickedSources.map((s) => (
                      <li key={s.id}>
                        <SourceIcon source={s} size="sm" />
                        <span className="mono">{s.name}</span>
                      </li>
                    ))}
                  </ol>
                ) : (
                  "None yet"
                ),
              },
              {
                label: "Default publish target",
                value: locals.find((s) => s.id === writeTarget) ? (
                  <span className="titled">
                    <SourceIcon
                      source={locals.find((s) => s.id === writeTarget)!}
                      size="sm"
                    />
                    <span className="mono">
                      {locals.find((s) => s.id === writeTarget)?.name}
                    </span>
                  </span>
                ) : (
                  "Reject writes"
                ),
              },
            ]}
          />
          <CodeBlock
            title="Client setup"
            code={pullCommand(format, slug, "<package>", "")}
          />
          <Failure error={action.error} />
        </>
      )}
    </Modal>
  );
}

function FeedDetail({
  client,
  id,
  back,
  setCrumb,
  openPackage,
  go,
}: {
  client: Client;
  id: string;
  back: () => void;
  setCrumb: (s: string) => void;
  openPackage: (p: PackageRef) => void;
  go: Go;
}) {
  const [tab, setTab] = useState("sources");
  const [epoch, setEpoch] = useState(0);
  const r = useResource(
    () => client.call("getFeed", { params: { id } }),
    [client, id, epoch],
  );
  const refresh = () => setEpoch((v) => v + 1);
  const feed = r.value?.data;
  useEffect(() => {
    if (feed) setCrumb(feed.slug);
  }, [feed?.slug]);
  const [deleting, setDeleting] = useState(false);
  return (
    <>
      <PageHeading
        back={{ label: "Feeds", onClick: back }}
        eyebrow={feed ? `${feed.format} feed` : "Feed"}
        title={
          feed ? (
            <>
              <span className="mono title-mono">{feed.slug}</span>
              {isCache(feed) && <CacheBadge />}
              {feed.anonymous_read ? (
                <Badge tone="warn" icon="globe">
                  Anonymous read
                </Badge>
              ) : (
                <Badge icon="lock">Authenticated</Badge>
              )}
            </>
          ) : (
            "Feed configuration"
          )
        }
        text={
          feed ? (
            <>
              Revision {feed.revision}
              {isCache(feed) && feed.cache_policy && (
                <>
                  {" · "}
                  {policySummary(feed.cache_policy)}
                </>
              )}
              {" · "}
              <Copyable text={endpoint(feed.slug)} />
            </>
          ) : undefined
        }
      >
        <Button
          variant="secondary"
          icon="refresh-cw"
          disabled={r.loading}
          onClick={refresh}
        >
          Refresh
        </Button>
        {feed && (
          <DropdownMenu
            align="end"
            trigger={
              <Button variant="secondary" iconRight="chevron-down">
                Actions
              </Button>
            }
            items={[
              {
                label: "Copy feed ID",
                icon: "copy",
                onSelect: () => void navigator.clipboard?.writeText(feed.id),
              },
              { divider: true },
              {
                label: "Delete feed",
                icon: "trash-2",
                danger: true,
                onSelect: () => setDeleting(true),
              },
            ]}
          />
        )}
      </PageHeading>
      <Failure error={r.error} />
      {r.loading && <Loading what="feed" />}
      {feed && (
        <>
          <Tabs
            value={tab}
            onChange={setTab}
            className="spaced-tabs"
            items={[
              { id: "sources", label: "Read order" },
              { id: "policy", label: "Publish policy" },
              { id: "packages", label: "Packages" },
              { id: "connect", label: "Connect" },
              { id: "settings", label: "Settings" },
            ]}
          />
          {tab === "sources" && (
            <FeedSources
              key={"s" + epoch}
              client={client}
              feed={feed}
              go={go}
              refresh={refresh}
            />
          )}
          {tab === "policy" && (
            <PolicyTab key={"p" + epoch} client={client} feed={feed} />
          )}
          {tab === "packages" && (
            <Card flush className="pad">
              <FeedPackages
                client={client}
                feed={feed}
                openPackage={openPackage}
              />
            </Card>
          )}
          {tab === "connect" && <Connect feed={feed} />}
          {tab === "settings" && (
            <FeedSettings
              key={"f" + epoch}
              client={client}
              feed={feed}
              etag={r.value?.etag ?? null}
              refresh={refresh}
              askDelete={() => setDeleting(true)}
            />
          )}
          {deleting && (
            <DeleteFeed
              client={client}
              feed={feed}
              etag={r.value?.etag ?? null}
              onClose={() => setDeleting(false)}
              deleted={back}
            />
          )}
        </>
      )}
    </>
  );
}

function Connect({ feed }: { feed: Feed }) {
  return (
    <div className="grid-auto grid-auto--340">
      <Card title="1 · Authenticate">
        <CodeBlock
          title="shell"
          code={
            feed.format === "oci"
              ? `docker login ${location.host} -u <user> --password-stdin < token.txt`
              : "export ANT_TOKEN=<service token>"
          }
        />
        <p className="muted small">
          Authenticate with a service token, a personal access token (Settings,
          Tokens), a directory password or a GitHub Actions identity, as this
          feed allows.
        </p>
      </Card>
      <Card title="2 · Pull and publish">
        <CodeBlock
          title="shell"
          code={pullCommand(feed.format, feed.slug, "<package>", "<version>")}
        />
        <p className="muted small">
          The API does not report which protocols this deployment serves.
        </p>
      </Card>
    </div>
  );
}

function FeedSettings({
  client,
  feed,
  etag,
  refresh,
  askDelete,
}: {
  client: Client;
  feed: Feed;
  etag: string | null;
  refresh: () => void;
  askDelete: () => void;
}) {
  const [slug, setSlug] = useState(feed.slug);
  const [anon, setAnon] = useState(feed.anonymous_read);
  const [purpose, setPurpose] = useState<"artifacts" | "cache">(feed.purpose);
  const [draft, setDraft] = useDraft(feed.cache_policy);
  const action = useAction();
  const toast = useToast();
  const cacheInput = purpose === "cache" ? draftToInput(draft) : undefined;
  const valid =
    slugPattern.test(slug) && (purpose !== "cache" || cacheInput !== undefined);
  return (
    <div className="narrow">
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          if (!valid) return;
          const ok = await action.run(async () => {
            await client.call("updateFeed", {
              params: { id: feed.id },
              etag: requireETag(etag),
              body: {
                slug,
                anonymous_read: anon,
                ...(purpose !== feed.purpose ? { purpose } : {}),
                ...(purpose === "cache" ? { cache_policy: cacheInput } : {}),
              },
            });
          });
          if (ok) {
            toast("Feed saved", slug);
            refresh();
          }
        }}
      >
        <SettingsSection
          title="General"
          form
          footer={
            <Button type="submit" disabled={action.busy || !valid}>
              {action.busy ? "Saving…" : "Save feed"}
            </Button>
          }
          footerNote={`Saved with If-Match: ${etag ?? "—"}. A newer change elsewhere blocks this save.`}
        >
          <Input
            label="Feed slug"
            mono
            required
            maxLength={63}
            value={slug}
            onChange={val(setSlug)}
            error={
              valid ? undefined : "Lowercase letters, digits, _ or -; up to 63."
            }
            hint="Renaming changes every client URL."
          />
          <Select
            label="Purpose"
            options={[
              { value: "artifacts", label: "Artifacts (published content)" },
              { value: "cache", label: "Build cache (buildx, sccache)" },
            ]}
            value={purpose}
            disabled={
              feed.purpose !== "cache" && !cacheFormats.includes(feed.format)
            }
            onChange={(e: { target: { value: string } }) =>
              setPurpose(e.target.value as "artifacts" | "cache")
            }
            hint={
              cacheFormats.includes(feed.format)
                ? "Changing the purpose needs feed administration. A cache feed needs local sources used by no other feed and no replication."
                : "Only oci and raw feeds can be build caches."
            }
          />
          {purpose === "cache" && (
            <CachePolicyFields draft={draft} set={setDraft} />
          )}
          <SettingsRow
            label="Anonymous reads"
            description="Anyone who can reach the endpoint can pull. The server enforces resource permissions."
          >
            <Switch
              aria-label="Allow anonymous reads"
              checked={anon}
              onChange={chk(setAnon)}
            />
          </SettingsRow>
          <Failure error={action.error} />
        </SettingsSection>
      </form>
      <FeedAccess client={client} feedId={feed.id} />
      <SettingsSection
        title="Delete feed"
        description="Removes the endpoint. Sources and their stored content are kept."
        danger
      >
        <SettingsRow label={feed.slug} description="This cannot be undone.">
          <Button variant="danger" size="sm" icon="trash-2" onClick={askDelete}>
            Delete
          </Button>
        </SettingsRow>
      </SettingsSection>
    </div>
  );
}

function DeleteFeed({
  client,
  feed,
  etag,
  onClose,
  deleted,
}: {
  client: Client;
  feed: Feed;
  etag: string | null;
  onClose: () => void;
  deleted: () => void;
}) {
  const [confirm, setConfirm] = useState("");
  const [unavailable, setUnavailable] = useState(false);
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Delete feed"
      description="Removes the endpoint. Sources and their stored content are kept."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            icon="trash-2"
            disabled={confirm !== feed.slug || action.busy || unavailable}
            onClick={async () => {
              setUnavailable(false);
              const ok = await action.run(async () => {
                try {
                  await client.call("deleteFeed", {
                    params: { id: feed.id },
                    etag: requireETag(etag),
                  });
                } catch (e) {
                  if (isUnavailable(e)) {
                    setUnavailable(true);
                    return;
                  }
                  throw e;
                }
                toast("Feed deleted", feed.slug);
                deleted();
              });
              void ok;
            }}
          >
            Delete feed
          </Button>
        </>
      }
    >
      <Input
        label={`Type ${feed.slug} to confirm`}
        mono
        value={confirm}
        onChange={val(setConfirm)}
        autoComplete="off"
      />
      {unavailable && <Unavailable what="Feed deletion" compact />}
      <Failure error={action.error} />
    </Modal>
  );
}
