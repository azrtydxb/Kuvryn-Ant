import { SignIn } from "./SignIn";
import { useSession } from "./useSession";
import { Identity } from "./Identity";
import { useResource, Failure, Field, Format, Mutation, Reload } from "./ui";
import { useEffect, useId, useMemo, useState } from "react";
import { Client, requireETag } from "./api";
import {
  type Feed,
  type Source,
  type PublishPolicy,
  type PublishRule,
  type SourceAttachmentPage,
} from "./generated";

const notice = (
  <p className="notice">
    Formats listed here are configuration options. The API does not report
    deployed protocol capabilities; a configured feed does not establish working
    publish, pull-through, or replica support.
  </p>
);
function Status({ client }: { client: Client }) {
  const r = useResource(() => client.call("getStatus"), [client]);
  const [checked, setChecked] = useState("");
  useEffect(() => {
    if (r.value) setChecked(new Date().toLocaleString());
  }, [r.value]);
  return (
    <>
      <div className="heading">
        <div>
          <p className="eyebrow">INSTANCE</p>
          <h1>Overview</h1>
        </div>
        <Reload {...r} />
      </div>
      <p className="intro">A clear view of your artifact service.</p>
      <section className="card">
        <h2>Management API</h2>
        <Failure error={r.error} />
        {r.loading ? (
          <p role="status">Checking service…</p>
        ) : (
          r.value && (
            <>
              <p className="status">
                <span className="dot" />
                Reachable
              </p>
              <dl>
                <dt>Service</dt>
                <dd>{r.value.data.name}</dd>
                <dt>API version</dt>
                <dd>{r.value.data.api_version}</dd>
                <dt>Last successful check</dt>
                <dd>{checked}</dd>
              </dl>
            </>
          )
        )}
        <p className="muted">
          Reachability does not establish database, storage, or adapter
          readiness.
        </p>
      </section>
      <section className="card">
        <h2>Available in this console</h2>
        <p>
          Manage feeds, source configuration, read priority, and publish routing
          through the versioned management API.
        </p>
        {notice}
        <p className="muted">
          Capacity, cache acceleration, upstream health, replication, package
          browsing are not exposed by this management contract.
        </p>
      </section>
    </>
  );
}
function FeedForm({
  client,
  feed,
  etag,
  done,
}: {
  client: Client;
  feed?: Feed;
  etag?: string | null;
  done: () => void;
}) {
  const [slug, setSlug] = useState(feed?.slug ?? "");
  const [format, setFormat] = useState<Feed["format"]>(feed?.format ?? "oci");
  const [anonymous, setAnonymous] = useState(feed?.anonymous_read ?? false);
  return (
    <Mutation
      label={feed ? "Save feed" : "Create feed"}
      run={async () => {
        if (feed)
          await client.call("updateFeed", {
            params: { id: feed.id },
            etag: requireETag(etag ?? null),
            body: { slug, anonymous_read: anonymous },
          });
        else
          await client.call("createFeed", {
            body: { slug, format, anonymous_read: anonymous },
          });
        done();
      }}
    >
      <Field
        label="Feed slug"
        hint="Lowercase letters, digits, underscores, or hyphens; up to 63 characters."
      >
        <input
          required
          pattern="[a-z0-9][a-z0-9_\-]{0,62}"
          maxLength={63}
          value={slug}
          onChange={(e) => setSlug(e.target.value)}
        />
      </Field>
      {!feed && <Format value={format} onChange={setFormat} />}
      <label className="check">
        <input
          type="checkbox"
          checked={anonymous}
          onChange={(e) => setAnonymous(e.target.checked)}
        />
        Allow anonymous reads
      </label>
      <p className="muted">
        Anonymous access is off by default. The server enforces resource
        permissions.
      </p>
    </Mutation>
  );
}
function Feeds({ client }: { client: Client }) {
  const [cursor, setCursor] = useState("");
  const [filter, setFilter] = useState("");
  const [prefix, setPrefix] = useState("");
  const [selected, setSelected] = useState("");
  const [creating, setCreating] = useState(false);
  const r = useResource(
    () => client.call("listFeeds", { query: { limit: "50", cursor, prefix } }),
    [client, cursor, prefix],
  );
  if (selected)
    return (
      <FeedDetail
        client={client}
        id={selected}
        back={() => {
          setSelected("");
          r.reload();
        }}
      />
    );
  return (
    <>
      <div className="heading">
        <div>
          <p className="eyebrow">REPOSITORIES</p>
          <h1>Feeds</h1>
        </div>
        <button className="primary" onClick={() => setCreating((v) => !v)}>
          {creating ? "Close create form" : "Create feed"}
        </button>
      </div>
      <p className="intro">
        Stable entry points for your packages and artifacts.
      </p>
      {notice}
      {creating && (
        <section className="card">
          <h2>Create feed</h2>
          <FeedForm
            client={client}
            done={() => {
              setCreating(false);
              setCursor("");
              r.reload();
            }}
          />
        </section>
      )}
      <form
        className="toolbar"
        onSubmit={(e) => {
          e.preventDefault();
          setCursor("");
          setPrefix(filter);
        }}
      >
        <Field label="Filter by slug prefix">
          <input
            maxLength={200}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
          />
        </Field>
        <button>Filter</button>
        <Reload {...r} />
      </form>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading feeds…</p>}
      {r.value && (
        <>
          <div className="card table-wrap">
            <table>
              <caption>Feeds visible to this token</caption>
              <thead>
                <tr>
                  <th>Feed</th>
                  <th>Format configuration</th>
                  <th>Access</th>
                  <th>Revision</th>
                </tr>
              </thead>
              <tbody>
                {r.value.data.items.map((f) => (
                  <tr key={f.id}>
                    <td>
                      <button
                        className="link"
                        onClick={() => setSelected(f.id)}
                      >
                        {f.slug}
                      </button>
                    </td>
                    <td>
                      <span className="badge">{f.format}</span>
                    </td>
                    <td>
                      {f.anonymous_read ? "Anonymous reads" : "Authenticated"}
                    </td>
                    <td>{f.revision}</td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!r.value.data.items.length && <p>No feeds match this view.</p>}
          </div>
          <Pager
            cursor={cursor}
            page={r.value.data.page}
            setCursor={setCursor}
          />
        </>
      )}
    </>
  );
}
function Pager({
  cursor,
  page,
  setCursor,
}: {
  cursor: string;
  page: { has_more: boolean; next_cursor?: string };
  setCursor: (s: string) => void;
}) {
  return (
    <div className="pager">
      <button disabled={!cursor} onClick={() => setCursor("")}>
        First page
      </button>
      <button
        disabled={!page.has_more || !page.next_cursor}
        onClick={() => setCursor(page.next_cursor!)}
      >
        Next page
      </button>
    </div>
  );
}
function Sources({ client }: { client: Client }) {
  const [cursor, setCursor] = useState("");
  const [prefix, setPrefix] = useState("");
  const [filter, setFilter] = useState("");
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState("");
  const [kind, setKind] = useState<Source["kind"]>("local");
  const [format, setFormat] = useState<Source["format"]>("oci");
  const r = useResource(
    () =>
      client.call("listSources", { query: { limit: "50", cursor, prefix } }),
    [client, cursor, prefix],
  );
  return (
    <>
      <div className="heading">
        <div>
          <p className="eyebrow">CONTENT ORIGINS</p>
          <h1>Sources</h1>
        </div>
        <button className="primary" onClick={() => setCreating((v) => !v)}>
          {creating ? "Close create form" : "Create source"}
        </button>
      </div>
      <p className="intro">
        Configure content origins, then attach them to a matching feed.
      </p>
      {notice}
      {creating && (
        <section className="card">
          <h2>Create source</h2>
          <Mutation
            label="Create source"
            run={async () => {
              if (new TextEncoder().encode(name.trim()).length > 200)
                throw Error("Source name must be at most 200 UTF-8 bytes.");
              await client.call("createSource", {
                body: { name: name.trim(), kind, format },
              });
              setName("");
              setCreating(false);
              setCursor("");
              r.reload();
            }}
          >
            <Field label="Source name">
              <input
                value={name}
                required
                maxLength={200}
                onChange={(e) => setName(e.target.value)}
              />
            </Field>
            <Format value={format} onChange={setFormat} />
            <Field label="Source kind">
              <select
                value={kind}
                onChange={(e) => setKind(e.target.value as Source["kind"])}
              >
                {["local", "remote", "replica"].map((k) => (
                  <option key={k}>{k}</option>
                ))}
              </select>
            </Field>
            <p className="muted">
              This creates configuration only. Upstream addresses, credentials,
              and replication setup are not accepted by this API.
            </p>
          </Mutation>
        </section>
      )}
      <form
        className="toolbar"
        onSubmit={(e) => {
          e.preventDefault();
          setCursor("");
          setPrefix(filter);
        }}
      >
        <Field label="Filter by source name prefix">
          <input
            value={filter}
            maxLength={200}
            onChange={(e) => setFilter(e.target.value)}
          />
        </Field>
        <button>Filter</button>
        <Reload {...r} />
      </form>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading sources…</p>}
      {r.value && (
        <>
          <div className="card table-wrap">
            <table>
              <caption>Sources visible to this token</caption>
              <thead>
                <tr>
                  <th>Name / source ID</th>
                  <th>Kind</th>
                  <th>Format configuration</th>
                </tr>
              </thead>
              <tbody>
                {r.value.data.items.map((s) => (
                  <tr key={s.id}>
                    <td>
                      <strong>{s.name}</strong>
                      <code>{s.id}</code>
                    </td>
                    <td>{s.kind}</td>
                    <td>
                      <span className="badge">{s.format}</span>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
            {!r.value.data.items.length && <p>No sources match this view.</p>}
          </div>
          <Pager
            cursor={cursor}
            page={r.value.data.page}
            setCursor={setCursor}
          />
        </>
      )}
    </>
  );
}
function FeedDetail({
  client,
  id,
  back,
}: {
  client: Client;
  id: string;
  back: () => void;
}) {
  const [epoch, setEpoch] = useState(0);
  const r = useResource(
    () => client.call("getFeed", { params: { id } }),
    [client, id, epoch],
  );
  const refresh = () => setEpoch((v) => v + 1);
  return (
    <>
      <button className="link" onClick={back}>
        ← All feeds
      </button>
      <div className="heading">
        <h1>{r.value?.data.slug ?? "Feed configuration"}</h1>
        <Reload loading={r.loading} reload={refresh} />
      </div>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading feed…</p>}
      {r.value && (
        <>
          <p className="intro">
            {r.value.data.format} configuration · Revision{" "}
            {r.value.data.revision}
          </p>
          <section className="card">
            <h2>Feed settings</h2>
            <FeedForm
              key={epoch}
              client={client}
              feed={r.value.data}
              etag={r.value.etag}
              done={refresh}
            />
          </section>
          <Attachments
            key={"sources" + epoch}
            client={client}
            id={id}
            refresh={refresh}
          />
          <Policy
            key={"policy" + epoch}
            client={client}
            id={id}
            refresh={refresh}
          />
        </>
      )}
    </>
  );
}
const uuidPattern =
  "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
function Attachments({
  client,
  id,
  refresh,
}: {
  client: Client;
  id: string;
  refresh: () => void;
}) {
  const [cursor, setCursor] = useState("");
  const [source, setSource] = useState("");
  const [priority, setPriority] = useState(0);
  const r = useResource(
    () =>
      client.call("listFeedSources", {
        params: { id },
        query: { limit: "50", cursor },
      }),
    [client, id, cursor],
  );
  async function attach(source_id: string, p: number) {
    await client.call("attachFeedSource", {
      params: { id, source_id },
      etag: requireETag(r.value?.etag ?? null),
      body: { priority: p },
    });
    refresh();
  }
  return (
    <section className="card">
      <div className="heading">
        <h2>Read source order</h2>
        <Reload {...r} />
      </div>
      <p>
        Lower priorities are read first. Only sources you can read are shown;
        this may not be the complete order. Source ownership remains
        authoritative after resolution.
      </p>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading attachments…</p>}
      {r.value && (
        <>
          <ol className="attachments">
            {r.value.data.items.map((a) => (
              <li key={a.source.id}>
                <Priority item={a} save={(p) => attach(a.source.id, p)} />
              </li>
            ))}
          </ol>
          {!r.value.data.items.length && <p>No visible attached sources.</p>}
          <Pager
            cursor={cursor}
            page={r.value.data.page}
            setCursor={setCursor}
          />
          <details>
            <summary>Attach a source</summary>
            <Mutation
              label="Attach source"
              run={() => attach(source, priority)}
            >
              <Field
                label="Source UUID"
                hint="Copy the ID from Sources. The source format must match this feed."
              >
                <input
                  required
                  pattern={uuidPattern}
                  value={source}
                  onChange={(e) => setSource(e.target.value)}
                />
              </Field>
              <Field
                label="Read priority"
                hint="Use an unoccupied priority between 0 and 1,000,000."
              >
                <input
                  type="number"
                  min={0}
                  max={1000000}
                  required
                  value={priority}
                  onChange={(e) => setPriority(Number(e.target.value))}
                />
              </Field>
            </Mutation>
          </details>
        </>
      )}
    </section>
  );
}
function Priority({
  item,
  save,
}: {
  item: SourceAttachmentPage["items"][number];
  save: (p: number) => Promise<void>;
}) {
  const [priority, setPriority] = useState(item.priority);
  return (
    <Mutation label="Update priority" run={() => save(priority)}>
      <strong>{item.source.name}</strong>
      <span className="badge">{item.source.kind}</span>
      <code>{item.source.id}</code>
      <Field label={`Read priority for ${item.source.name}`}>
        <input
          type="number"
          min={0}
          max={1000000}
          required
          value={priority}
          onChange={(e) => setPriority(Number(e.target.value))}
        />
      </Field>
    </Mutation>
  );
}
function Policy({
  client,
  id,
  refresh,
}: {
  client: Client;
  id: string;
  refresh: () => void;
}) {
  const r = useResource(
    () => client.call("getPublishPolicy", { params: { id } }),
    [client, id],
  );
  return (
    <section className="card">
      <div className="heading">
        <h2>Publish policy</h2>
        <Reload {...r} />
      </div>
      <p>
        Deny rules override allows. Otherwise, the highest matching priority
        selects one attached local source. With no match, the default applies;
        without a default the write is rejected.
      </p>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading publish policy…</p>}
      {r.value && (
        <PolicyEditor
          client={client}
          policy={r.value.data}
          etag={r.value.etag}
          refresh={refresh}
        />
      )}
    </section>
  );
}
function PolicyEditor({
  client,
  policy,
  etag,
  refresh,
}: {
  client: Client;
  policy: PublishPolicy;
  etag: string | null;
  refresh: () => void;
}) {
  const [defaultID, setDefaultID] = useState(policy.default_source_id);
  const [rules, setRules] = useState<PublishRule[]>(policy.rules);
  const group = useId();
  const update = (index: number, rule: PublishRule) =>
    setRules((current) => current.map((r, i) => (i === index ? rule : r)));
  return (
    <Mutation
      label="Save publish policy"
      run={async () => {
        if (new Set(rules.map((r) => r.id)).size !== rules.length)
          throw Error("Rule IDs must be unique.");
        if (rules.some((r) => !Number.isSafeInteger(r.priority)))
          throw Error("Rule priorities must be safe integers.");
        await client.call("setPublishPolicy", {
          params: { id: policy.feed_id },
          etag: requireETag(etag),
          body: { default_source_id: defaultID, rules },
        });
        refresh();
      }}
    >
      <Field
        label="Default write target — source UUID"
        hint="Leave empty to reject unmatched writes. Every target must be attached, local, and the same format."
      >
        <input
          pattern={uuidPattern}
          value={defaultID}
          onChange={(e) => setDefaultID(e.target.value)}
        />
      </Field>
      <p className="notice">
        Default destination:{" "}
        <strong>{defaultID || "Reject unmatched writes"}</strong>. Saving
        replaces the complete policy below.
      </p>
      {rules.map((rule, i) => (
        <fieldset className="rule" key={`${group}-${i}`}>
          <legend>Rule {i + 1}</legend>
          <div className="grid">
            <Field label="Rule ID">
              <input
                required
                maxLength={128}
                pattern="[A-Za-z0-9_.:\-]+"
                value={rule.id}
                onChange={(e) => update(i, { ...rule, id: e.target.value })}
              />
            </Field>
            <Field label="Match priority (highest wins)">
              <input
                type="number"
                required
                min={Number.MIN_SAFE_INTEGER}
                max={Number.MAX_SAFE_INTEGER}
                value={rule.priority}
                onChange={(e) =>
                  update(i, { ...rule, priority: Number(e.target.value) })
                }
              />
            </Field>
          </div>
          <label className="check">
            <input
              type="checkbox"
              checked={rule.deny}
              onChange={(e) => {
                const next = { ...rule, deny: e.target.checked };
                if (next.deny) delete next.target_source_id;
                update(i, next);
              }}
            />
            Deny matching writes
          </label>
          {!rule.deny && (
            <Field label="Write target — local source UUID">
              <input
                required
                pattern={uuidPattern}
                value={rule.target_source_id ?? ""}
                onChange={(e) =>
                  update(i, { ...rule, target_source_id: e.target.value })
                }
              />
            </Field>
          )}
          <p className="muted">
            All populated selectors must match; empty fields match any value.
            Identity matches are exact and case-sensitive.
          </p>
          <div className="grid">
            {(
              ["namespace", "package", "path_prefix", "principal_id"] as const
            ).map((key) => (
              <Field key={key} label={key.replaceAll("_", " ")}>
                <input
                  maxLength={key === "path_prefix" ? 2048 : 256}
                  value={rule.selector[key] ?? ""}
                  onChange={(e) =>
                    update(i, {
                      ...rule,
                      selector: { ...rule.selector, [key]: e.target.value },
                    })
                  }
                />
              </Field>
            ))}
          </div>
          <p>
            Matched destination:{" "}
            <strong>
              {rule.deny
                ? "Reject write"
                : rule.target_source_id || "Select a local source"}
            </strong>
          </p>
          <button
            type="button"
            onClick={() => setRules(rules.filter((_, n) => n !== i))}
          >
            Remove rule {i + 1}
          </button>
        </fieldset>
      ))}
      <button
        type="button"
        disabled={rules.length >= 256}
        onClick={() =>
          setRules([
            ...rules,
            {
              id: "rule-" + crypto.randomUUID(),
              priority: 0,
              deny: false,
              selector: {},
            },
          ])
        }
      >
        Add rule
      </button>
      <p className="muted">
        The server validates ambiguous matches and target permissions before
        applying any change.
      </p>
    </Mutation>
  );
}
export default function App() {
  const auth = useSession();
  const { client } = auth;
  const [tab, setTab] = useState("overview");
  const publicClient = useMemo(() => new Client(), []);
  useEffect(() => {
    const heading = document.querySelector<HTMLElement>("main h1");
    if (heading) {
      heading.tabIndex = -1;
      heading.focus();
    }
  }, [tab, client]);
  return (
    <div className="shell">
      <a className="skip" href="#main">
        Skip to content
      </a>
      <aside>
        <a
          className="brand"
          href="#"
          onClick={(e) => {
            e.preventDefault();
            setTab("overview");
          }}
        >
          <span className="brand-mark">a</span>
          <span>
            Kuvryn Ant<small>ARTIFACT ADMINISTRATION</small>
          </span>
        </a>
        <nav aria-label="Main navigation">
          {["overview", "feeds", "sources", "identity"].map((t) => (
            <button
              key={t}
              aria-current={tab === t ? "page" : undefined}
              onClick={() => setTab(t)}
            >
              {t[0].toUpperCase() + t.slice(1)}
            </button>
          ))}
        </nav>
        <div className="session">
          <span className="badge">Management v1</span>
          <p>
            {auth.checking
              ? "Checking session…"
              : client
                ? auth.browser
                  ? "Browser session"
                  : "In-memory service token"
                : "Not signed in"}
          </p>
          {client && (
            <button
              disabled={auth.loggingOut}
              onClick={() => {
                setTab("overview");
                void auth.signOut();
              }}
            >
              Sign out
            </button>
          )}
        </div>
      </aside>
      <main id="main" tabIndex={-1}>
        <Failure error={auth.error} />
        {auth.checking && <p role="status">Checking your browser session…</p>}
        {auth.loggingOut && <p role="status">Signing out…</p>}
        {auth.logoutFailed && (
          <button
            onClick={() => void auth.signOut()}
            disabled={auth.loggingOut}
          >
            Retry server sign-out
          </button>
        )}
        {!client && !auth.checking && !auth.loggingOut && (
          <>
            <SignIn onSession={auth.accept} onToken={auth.useToken} />
            {auth.error && !auth.logoutFailed && (
              <button onClick={auth.recheck}>Retry session check</button>
            )}
          </>
        )}
        {tab === "overview" ? (
          <Status client={publicClient} />
        ) : !client ? (
          <>
            <h1 tabIndex={-1}>
              {tab === "feeds"
                ? "Feeds"
                : tab === "sources"
                  ? "Sources"
                  : "Identity administration"}
            </h1>
            <p>Sign in to load resources permitted by your grants.</p>
          </>
        ) : tab === "feeds" ? (
          <Feeds key="feeds" client={client} />
        ) : tab === "identity" ? (
          <Identity client={client} />
        ) : (
          <Sources key="sources" client={client} />
        )}
        <footer>
          Kuvryn Ant · Configuration console · All changes are authorized by the
          service.
        </footer>
      </main>
    </div>
  );
}
