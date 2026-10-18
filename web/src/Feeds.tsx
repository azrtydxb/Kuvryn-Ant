import { useEffect, useState } from "react";
import { APIError, requireETag, type Client } from "./api";
import type { Go } from "./App";
import { useAllSources, sourcesForFormat } from "./data";
import { isUnavailable, paths, type FeedStats } from "./gaps";
import { formats, type Feed } from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  CodeBlock,
  DropdownMenu,
  EmptyState,
  Icon,
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
import { PolicyTab } from "./Policy";
import { FeedPackages, pullCommand, type PackageRef } from "./Packages";
import {
  Card,
  Copyable,
  Failure,
  Loading,
  Needs,
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
  const prefix = useDebounced(filter);
  const r = useResource(
    () =>
      client.call("listFeeds", {
        query: { limit: "50", ...(cursor ? { cursor } : {}), prefix },
      }),
    [client, cursor, prefix, epoch],
  );
  const rows = r.value?.data.items ?? [];
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
                    </button>
                  ),
                },
                {
                  key: "format",
                  label: "Format",
                  render: (f: Feed) => <Badge tone="outline">{f.format}</Badge>,
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
                  render: (f: Feed) => count((f as FeedStats).package_count),
                },
                {
                  key: "size",
                  label: "Stored",
                  align: "right",
                  render: (f: Feed) => bytes((f as FeedStats).size_bytes),
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
      <Needs block>per-feed package/size counts</Needs>
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
  const [anon, setAnon] = useState(false);
  const [picked, setPicked] = useState<string[]>([]);
  const [target, setTarget] = useState("");
  const [made, setMade] = useState("");
  const toast = useToast();
  const sources = useAllSources(client);
  const action = useAction();
  const valid = slugPattern.test(slug);
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
  // No batch endpoint yet: create the feed, then attach sources in read order
  // and set the default publish target. A failure after the feed exists
  // reports the step and offers to open the feed; nothing is rolled back.
  async function submit() {
    let id = made;
    let stage = "create the feed";
    const ok = await action.run(async () => {
      try {
        let revision = 0;
        if (!id) {
          const feed = await client.call("createFeed", {
            body: { slug, format, anonymous_read: anon },
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
            label="Format"
            options={formats.map((f) => ({ value: f, label: f }))}
            value={format}
            onChange={(e: { target: { value: string } }) => {
              setFormat(e.target.value as Feed["format"]);
              setPicked([]);
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
                  <Icon
                    name={s.kind === "remote" ? "cloud-download" : "hard-drive"}
                    size={16}
                  />
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
                label: "Access",
                value: anon ? "Anonymous read" : "Authenticated",
              },
              {
                label: "Read order",
                value: pickedSources.length
                  ? pickedSources.map((s) => s.name).join(" → ")
                  : "None yet",
                mono: true,
              },
              {
                label: "Default publish target",
                value:
                  locals.find((s) => s.id === writeTarget)?.name ??
                  "Reject writes",
                mono: true,
              },
            ]}
          />
          <CodeBlock
            title="Client setup"
            code={pullCommand(format, slug, "<package>", "")}
          />
          <Failure error={action.error} />
          <Needs block>
            batch create (feed + attachments + policy in one call)
          </Needs>
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
              Revision {feed.revision} · <Copyable text={endpoint(feed.slug)} />
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
          Use a service token for CI. Tokens are issued under Access.
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
  const action = useAction();
  const toast = useToast();
  const valid = slugPattern.test(slug);
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
              body: { slug, anonymous_read: anon },
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
      <Needs block>feed delete</Needs>
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
                  await client.extra("DELETE", paths.feed(feed.id), {
                    etag: requireETag(etag),
                  });
                } catch (e) {
                  if (isUnavailable(e) && e instanceof APIError) {
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
