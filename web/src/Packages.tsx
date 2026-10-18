import {
  createContext,
  useContext,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import type { Client } from "./api";
import { useAllFeeds, useAllSources } from "./data";
import { useOptionalCall } from "./gaps";
import type { DiscoveredAsset, Feed, PackageSearchResult } from "./generated";
import {
  Alert,
  Badge,
  Button,
  CodeBlock,
  Drawer,
  EmptyState,
  Input,
  Modal,
  PropertyList,
  SegmentedControl,
  Select,
  Table,
  Tabs,
} from "./ds/bundle";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  PageHeading,
  Pager,
  Toolbar,
  bytes,
  count,
  useAction,
  useDebounced,
  useResource,
  useToast,
  when,
} from "./ui";

// A package as the console tracks it: a discovered component plus the feed it
// was discovered through. Cross-feed search rows carry the feed themselves.
export type PackageRef = Partial<PackageSearchResult> & {
  id: string;
  coordinate: string;
  // Optional list-row extras some servers add.
  latest_version?: string;
  version_count?: number;
  updated_at?: string;
};

function PackageTable({
  rows,
  open,
  showFeed,
}: {
  rows: PackageRef[];
  open: (p: PackageRef) => void;
  showFeed: boolean;
}) {
  return (
    <Table
      onRowClick={open}
      columns={[
        {
          key: "coordinate",
          label: "Package",
          render: (r: PackageRef) => (
            <>
              {r.coordinate}
              <small>{r.namespace || "(root namespace)"}</small>
            </>
          ),
        },
        ...(showFeed
          ? [
              {
                key: "format",
                label: "Format",
                render: (r: PackageRef) =>
                  r.format ? <Badge tone="outline">{r.format}</Badge> : "—",
              },
              {
                key: "feed_slug",
                label: "Feed",
                mono: true,
                render: (r: PackageRef) => r.feed_slug ?? "—",
              },
              {
                key: "latest_version",
                label: "Latest",
                mono: true,
                render: (r: PackageRef) => r.latest_version ?? "—",
              },
              {
                key: "version_count",
                label: "Versions",
                align: "right",
                render: (r: PackageRef) => count(r.version_count),
              },
              {
                key: "updated_at",
                label: "Updated",
                align: "right",
                render: (r: PackageRef) => when(r.updated_at),
              },
            ]
          : []),
        {
          key: "source_id",
          label: "Served from",
          render: (r: PackageRef) => <SourceName id={r.source_id} />,
        },
      ]}
      rows={rows}
    />
  );
}
const SourceNames = createContext(new Map<string, string>());
function SourceName({ id }: { id?: string }) {
  const names = useContext(SourceNames);
  if (!id) return <>—</>;
  return <Mono>{names.get(id) ?? id}</Mono>;
}
function SourceNamesProvider({
  client,
  children,
}: {
  client: Client;
  children: ReactNode;
}) {
  const sources = useAllSources(client);
  const map = useMemo(
    () => new Map(sources.items.map((s) => [s.id, s.name])),
    [sources.items],
  );
  return <SourceNames.Provider value={map}>{children}</SourceNames.Provider>;
}

export function PackagesPage({
  client,
  openPackage,
}: {
  client: Client;
  openPackage: (p: PackageRef) => void;
}) {
  return (
    <>
      <PageHeading
        eyebrow="Content"
        title="Packages"
        text="Everything you can read through your feeds. Metadata only; shadowed sources do not fall through."
      />
      <SourceNamesProvider client={client}>
        <CrossFeed client={client} openPackage={openPackage} />
      </SourceNamesProvider>
    </>
  );
}

function CrossFeed({
  client,
  openPackage,
}: {
  client: Client;
  openPackage: (p: PackageRef) => void;
}) {
  const feeds = useAllFeeds(client);
  const [q, setQ] = useState("");
  const [feed, setFeed] = useState("");
  const [format, setFormat] = useState("");
  const [origin, setOrigin] = useState("all");
  const [cursor, setCursor] = useState("");
  const dq = useDebounced(q);
  const query: Record<string, string> = {
    limit: "50",
    ...(dq ? { q: dq } : {}),
    ...(feed ? { feed } : {}),
    ...(format ? { format } : {}),
    ...(origin !== "all" ? { origin } : {}),
    ...(cursor ? { cursor } : {}),
  };
  const search = useOptionalCall(client, "searchPackages", { query });
  if (search.state === "unavailable")
    return (
      <PerFeed client={client} feeds={feeds.items} openPackage={openPackage} />
    );
  const formats = [...new Set(feeds.items.map((f) => f.format))].sort();
  const rows = search.state === "ready" ? search.data.items : [];
  const total =
    search.state === "ready" ? (search.data.total ?? undefined) : undefined;
  return (
    <>
      <Toolbar
        right={
          search.state === "ready" ? (
            <>
              {rows.length}
              {total != null && ` of ${count(total)}`} packages
            </>
          ) : undefined
        }
      >
        <Input
          icon="search"
          size="sm"
          aria-label="Search packages"
          placeholder="Name, namespace or sha256:…"
          value={q}
          onChange={(e: { target: { value: string } }) => {
            setQ(e.target.value);
            setCursor("");
          }}
          className="w300"
        />
        <Select
          size="sm"
          aria-label="Feed"
          options={[
            { value: "", label: "All feeds" },
            ...feeds.items.map((f) => ({ value: f.id, label: f.slug })),
          ]}
          value={feed}
          onChange={(e: { target: { value: string } }) => {
            setFeed(e.target.value);
            setCursor("");
          }}
        />
        <Select
          size="sm"
          aria-label="Format"
          options={[
            { value: "", label: "All formats" },
            ...formats.map((f) => ({ value: f, label: f })),
          ]}
          value={format}
          onChange={(e: { target: { value: string } }) => {
            setFormat(e.target.value);
            setCursor("");
          }}
        />
        <SegmentedControl
          aria-label="Origin"
          options={[
            { value: "all", label: "All" },
            { value: "local", label: "Published" },
            { value: "remote", label: "Cached" },
          ]}
          value={origin}
          onChange={(v: string) => {
            setOrigin(v);
            setCursor("");
          }}
        />
      </Toolbar>
      {search.state === "loading" && <Loading what="packages" />}
      {search.state === "error" && <Failure error={search.error} />}
      {search.state === "ready" &&
        (rows.length ? (
          <>
            <PackageTable rows={rows} open={openPackage} showFeed />
            <Pager
              cursor={cursor}
              next={search.data.page.next_cursor}
              set={setCursor}
            />
          </>
        ) : (
          <EmptyState
            icon="search-x"
            title="No packages match"
            description="Nothing visible to you matches these filters. Shadowed sources do not fall through."
            action={
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  setQ("");
                  setFeed("");
                  setFormat("");
                  setOrigin("all");
                  setCursor("");
                }}
              >
                Clear filters
              </Button>
            }
          />
        ))}
    </>
  );
}

// Fallback until cross-feed search exists: the per-feed discovery API.
function PerFeed({
  client,
  feeds,
  openPackage,
}: {
  client: Client;
  feeds: Feed[];
  openPackage: (p: PackageRef) => void;
}) {
  const [feedId, setFeedId] = useState("");
  const selected = feeds.find((f) => f.id === feedId) ?? feeds[0];
  return (
    <>
      <Alert
        tone="info"
        title="Cross-feed search: Not available on this server yet"
        className="spaced"
      >
        Browsing one feed at a time. Choose a feed to list the packages visible
        through its source order.
      </Alert>
      {!feeds.length ? (
        <EmptyState
          icon="rss"
          title="No feeds to browse"
          description="Create a feed or ask for read access to one."
        />
      ) : (
        <FeedPackages
          key={selected.id}
          client={client}
          feed={selected}
          picker={
            <Select
              size="sm"
              aria-label="Feed"
              options={feeds.map((f) => ({ value: f.id, label: f.slug }))}
              value={selected.id}
              onChange={(e: { target: { value: string } }) =>
                setFeedId(e.target.value)
              }
            />
          }
          openPackage={openPackage}
        />
      )}
    </>
  );
}

export function FeedPackages({
  client,
  feed,
  picker,
  openPackage,
}: {
  client: Client;
  feed: Feed;
  picker?: ReactNode;
  openPackage: (p: PackageRef) => void;
}) {
  const [cursor, setCursor] = useState("");
  const [prefix, setPrefix] = useState("");
  const [namespace, setNamespace] = useState("");
  const [digest, setDigest] = useState("");
  const [query, setQuery] = useState<Record<string, string>>({});
  const r = useResource(
    () =>
      client.call("listPackages", {
        params: { id: feed.id },
        query: { limit: "25", ...(cursor ? { cursor } : {}), ...query },
      }),
    [client, feed.id, cursor, query],
  );
  const digestOK = !digest || /^sha256:[0-9a-f]{64}$/.test(digest);
  return (
    <>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!digestOK) return;
          setCursor("");
          setQuery({
            ...(prefix ? { prefix } : {}),
            ...(namespace ? { namespace } : {}),
            ...(digest ? { digest } : {}),
          });
        }}
      >
        <Toolbar
          right={
            r.value ? `${r.value.data.items.length} on this page` : undefined
          }
        >
          {picker}
          <Input
            size="sm"
            aria-label="Package prefix"
            placeholder="Package prefix"
            maxLength={200}
            value={prefix}
            onChange={(e: { target: { value: string } }) =>
              setPrefix(e.target.value)
            }
          />
          <Input
            size="sm"
            aria-label="Exact namespace"
            placeholder="Exact namespace"
            maxLength={256}
            value={namespace}
            onChange={(e: { target: { value: string } }) =>
              setNamespace(e.target.value)
            }
          />
          <Input
            size="sm"
            mono
            aria-label="SHA-256 checksum"
            placeholder="sha256:…"
            error={digestOK ? undefined : "sha256: followed by 64 hex digits"}
            value={digest}
            onChange={(e: { target: { value: string } }) =>
              setDigest(e.target.value)
            }
          />
          <Button size="sm" type="submit" icon="search">
            Search packages
          </Button>
        </Toolbar>
      </form>
      <Failure error={r.error} />
      {r.loading && <Loading what="packages" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <PackageTable
              showFeed={false}
              open={(p) =>
                openPackage({
                  ...p,
                  feed_id: feed.id,
                  feed_slug: feed.slug,
                  format: feed.format,
                })
              }
              rows={r.value.data.items}
            />
            <Pager
              cursor={cursor}
              next={r.value.data.page.next_cursor}
              set={setCursor}
            />
          </>
        ) : (
          <EmptyState
            icon="search-x"
            title="No packages match"
            description="No visible packages match these filters. Shadowed sources do not fall through."
          />
        ))}
    </>
  );
}

export function pullCommand(
  format: string,
  feed: string,
  name: string,
  version: string,
) {
  const host = location.host;
  if (format === "oci")
    return `docker pull ${host}/${feed}/${name}${version ? ":" + version : ""}`;
  if (format === "go")
    return `GOPROXY=https://${host}/feeds/${feed}/go \\\n  go get ${name}${version ? "@" + version : ""}`;
  if (format === "helm")
    return `helm repo add ${feed} https://${host}/feeds/${feed}/helm\nhelm install release ${feed}/${name}${version ? " --version " + version : ""}`;
  if (format === "cargo")
    return `cargo add ${name}${version ? "@" + version : ""} --registry ${feed}`;
  if (format === "raw")
    return `curl -fLO -H "Authorization: Bearer $ANT_TOKEN" \\\n  https://${host}/feeds/${feed}/raw/${name}`;
  return `https://${host}/feeds/${feed}/${format}/`;
}

export function PackageDrawer({
  client,
  pkg,
  onClose,
  openFeed,
}: {
  client: Client;
  pkg?: PackageRef;
  onClose: () => void;
  openFeed: (id: string) => void;
}) {
  if (!pkg) return null;
  return (
    <PackageDetail
      key={pkg.id}
      client={client}
      pkg={pkg}
      onClose={onClose}
      openFeed={openFeed}
    />
  );
}
function PackageDetail({
  client,
  pkg,
  onClose,
  openFeed,
}: {
  client: Client;
  pkg: PackageRef;
  onClose: () => void;
  openFeed: (id: string) => void;
}) {
  const [tab, setTab] = useState("versions");
  const [versionCursor, setVersionCursor] = useState("");
  const [tagCursor, setTagCursor] = useState("");
  const [version, setVersion] = useState("");
  const feedId = pkg.feed_id ?? "";
  const feed = useResource(
    () =>
      feedId
        ? client.call("getFeed", { params: { id: feedId } })
        : Promise.resolve({ data: undefined, etag: null }),
    [client, feedId],
  );
  const params = { id: feedId, component: pkg.id };
  const versions = useResource(
    () =>
      client.call("listPackageVersions", {
        params,
        query: {
          limit: "25",
          ...(versionCursor ? { cursor: versionCursor } : {}),
        },
      }),
    [client, feedId, pkg.id, versionCursor],
  );
  const tags = useResource(
    () =>
      client.call("listPackageTags", {
        params,
        query: { limit: "100", ...(tagCursor ? { cursor: tagCursor } : {}) },
      }),
    [client, feedId, pkg.id, tagCursor],
  );
  const sourceNames = useAllSources(client);
  const sourceName =
    sourceNames.items.find((s) => s.id === pkg.source_id)?.name ??
    pkg.source_id;
  const tagsByVersion = new Map<string, string[]>();
  for (const t of tags.value?.data.items ?? [])
    tagsByVersion.set(t.version_id, [
      ...(tagsByVersion.get(t.version_id) ?? []),
      t.name,
    ]);
  const latest = versions.value?.data.items[0];
  const slug = feed.value?.data?.slug ?? pkg.feed_slug ?? "";
  const format = feed.value?.data?.format ?? pkg.format ?? "";
  return (
    <Drawer
      open
      title={pkg.coordinate}
      description={
        <span className="row">
          {format && <Badge tone="outline">{format}</Badge>}
          {slug && <Mono muted>{slug}</Mono>}
        </span>
      }
      onClose={onClose}
      width={640}
      footer={
        <>
          <Button
            variant="secondary"
            icon="external-link"
            disabled={!feedId}
            onClick={() => openFeed(feedId)}
          >
            Open in feed
          </Button>
          <Button onClick={onClose}>Done</Button>
        </>
      }
    >
      {slug && format && (
        <CodeBlock
          title="Install"
          code={pullCommand(
            format,
            slug,
            pkg.coordinate,
            latest?.version ?? "",
          )}
        />
      )}
      <Tabs
        value={tab}
        onChange={setTab}
        items={[
          { id: "versions", label: "Versions" },
          { id: "details", label: "Details" },
          { id: "provenance", label: "Provenance" },
        ]}
      />
      {tab === "versions" && (
        <>
          <Failure error={versions.error ?? tags.error} />
          {versions.loading && <Loading what="versions" />}
          {versions.value &&
            (versions.value.data.items.length ? (
              <>
                <Table
                  onRowClick={(r: { id: string }) => setVersion(r.id)}
                  columns={[
                    {
                      key: "version",
                      label: "Version",
                      render: (r: { id: string; version: string }) => (
                        <>
                          <span className="mono">{r.version}</span>
                          <span className="row tight inline">
                            {(tagsByVersion.get(r.id) ?? []).map((t) => (
                              <Badge
                                key={t}
                                tone={t === "latest" ? "pillar" : "neutral"}
                              >
                                {t}
                              </Badge>
                            ))}
                          </span>
                        </>
                      ),
                    },
                    {
                      key: "created_at",
                      label: "Published",
                      render: (r: { created_at: string }) => when(r.created_at),
                    },
                    { key: "revision", label: "Revision", align: "right" },
                  ]}
                  rows={versions.value.data.items}
                />
                <Pager
                  cursor={versionCursor}
                  next={versions.value.data.page.next_cursor}
                  set={setVersionCursor}
                />
              </>
            ) : (
              <p className="muted">No visible versions.</p>
            ))}
          {tags.value?.data.page.next_cursor && (
            <Button
              size="sm"
              variant="ghost"
              onClick={() => setTagCursor(tags.value!.data.page.next_cursor!)}
            >
              More tags
            </Button>
          )}
          {version && versions.value && (
            <VersionActions
              key={"act" + version}
              client={client}
              feedId={feedId}
              component={pkg.id}
              version={
                versions.value.data.items.find((v) => v.id === version)
                  ?.version ?? ""
              }
              done={() => {
                setVersion("");
                setVersionCursor("");
                versions.reload();
              }}
            />
          )}
          {version && (
            <Assets
              key={version}
              client={client}
              feedId={feedId}
              component={pkg.id}
              version={version}
            />
          )}
        </>
      )}
      {tab === "details" && (
        <PropertyList
          items={[
            {
              label: "Namespace",
              value: pkg.namespace || "(root)",
              mono: true,
            },
            { label: "Served from", value: sourceName ?? "—", mono: true },
            { label: "Package ID", value: pkg.id, mono: true },
            {
              label: "Metadata",
              value: (
                <pre className="json">
                  {JSON.stringify(pkg.metadata ?? {}, null, 2)}
                </pre>
              ),
            },
          ]}
        />
      )}
      {tab === "provenance" && (
        <Provenance
          client={client}
          feedId={feedId}
          component={pkg.id}
          version={version || latest?.id || ""}
        />
      )}
    </Drawer>
  );
}
function Assets({
  client,
  feedId,
  component,
  version,
}: {
  client: Client;
  feedId: string;
  component: string;
  version: string;
}) {
  const [cursor, setCursor] = useState("");
  const r = useResource(
    () =>
      client.call("listPackageAssets", {
        params: { id: feedId, component, version },
        query: { limit: "25", ...(cursor ? { cursor } : {}) },
      }),
    [client, feedId, component, version, cursor],
  );
  return (
    <>
      <h3 className="h4">Version assets</h3>
      <Failure error={r.error} />
      {r.loading && <Loading what="assets" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              columns={[
                { key: "key", label: "Asset", mono: true },
                {
                  key: "digest",
                  label: "Digest",
                  render: (a: DiscoveredAsset) => <Copyable text={a.digest} />,
                },
                {
                  key: "size_bytes",
                  label: "Size",
                  align: "right",
                  render: (a: DiscoveredAsset) => bytes(a.size_bytes),
                },
                { key: "media_type", label: "Type", mono: true },
              ]}
              rows={r.value.data.items}
            />
            <Pager
              cursor={cursor}
              next={r.value.data.page.next_cursor}
              set={setCursor}
            />
          </>
        ) : (
          <p className="muted">No visible assets.</p>
        ))}
    </>
  );
}
function Provenance({
  client,
  feedId,
  component,
  version,
}: {
  client: Client;
  feedId: string;
  component: string;
  version: string;
}) {
  const [asset, setAsset] = useState("");
  const list = useResource(
    () =>
      version
        ? client.call("listPackageAssets", {
            params: { id: feedId, component, version },
            query: { limit: "50" },
          })
        : Promise.resolve({ data: undefined, etag: null }),
    [client, feedId, component, version],
  );
  const items = list.value?.data?.items ?? [];
  const chosen = asset || items[0]?.id || "";
  const record = useResource(
    () =>
      chosen
        ? client.call("getPackageAsset", {
            params: { id: feedId, asset: chosen },
          })
        : Promise.resolve({ data: undefined, etag: null }),
    [client, feedId, chosen],
  );
  if (!version)
    return <p className="muted">Select a version first on the Versions tab.</p>;
  return (
    <>
      <Failure error={list.error ?? record.error} />
      {(list.loading || record.loading) && <Loading what="provenance" />}
      {items.length > 1 && (
        <Select
          label="Asset"
          options={items.map((a) => ({ value: a.id, label: a.key }))}
          value={chosen}
          onChange={(e: { target: { value: string } }) =>
            setAsset(e.target.value)
          }
        />
      )}
      {!list.loading && !items.length && (
        <p className="muted">No visible assets for this version.</p>
      )}
      {record.value?.data && (
        <AssetRecord client={client} item={record.value.data} />
      )}
    </>
  );
}
export function AssetRecord({
  item,
  client,
}: {
  item: DiscoveredAsset;
  client: Client;
}) {
  const prov = useOptionalCall(client, "getAssetProvenance", {
    params: { id: item.id },
  });
  const p = prov.state === "ready" ? prov.data : undefined;
  return (
    <>
      <PropertyList
        items={[
          { label: "Key", value: item.key, mono: true },
          { label: "Source", value: item.source_id, mono: true },
          { label: "Checksum", value: <Copyable text={item.digest} /> },
          { label: "Size", value: `${item.size_bytes} bytes` },
          { label: "Content type", value: item.media_type, mono: true },
          { label: "Created", value: when(item.created_at) },
          {
            label: "Uploader",
            value: p?.uploader_subject ?? (item.uploader_id || "Not recorded"),
            mono: true,
          },
          ...(p
            ? [
                { label: "Origin", value: p.origin },
                {
                  label: "Token",
                  value: p.token_id ?? "—",
                  mono: true,
                },
                {
                  label: "Policy rule",
                  value: p.matched_rule ?? "—",
                  mono: true,
                },
                {
                  label: "Verified",
                  value: (
                    <Badge
                      tone={p.verification === "verified" ? "good" : "bad"}
                      icon={
                        p.verification === "verified"
                          ? "shield-check"
                          : "shield-alert"
                      }
                    >
                      {p.verification === "verified"
                        ? "sha256 match"
                        : "integrity failure"}
                    </Badge>
                  ),
                },
                {
                  label: "Downloads",
                  value: `${count(p.downloads)}${p.last_downloaded_at ? ` · last ${when(p.last_downloaded_at)}` : ""}`,
                },
              ]
            : []),
        ]}
      />
      {prov.state === "loading" && <Loading what="provenance" />}
      {prov.state === "error" && <Failure error={prov.error} />}
      {p && <p className="muted small">{p.verification_detail}</p>}
      <details>
        <summary>Recorded provenance metadata</summary>
        <pre className="json">{JSON.stringify(item.provenance, null, 2)}</pre>
      </details>
    </>
  );
}

// Delete and unlist for one version. The server enforces the feed delete
// policy (denied, unlist-only, immutable releases) and the delete permission;
// the console only confirms intent and shows the server's refusal.
function VersionActions({
  client,
  feedId,
  component,
  version,
  done,
}: {
  client: Client;
  feedId: string;
  component: string;
  version: string;
  done: () => void;
}) {
  const [mode, setMode] = useState<"" | "delete" | "unlist">("");
  const action = useAction();
  const toast = useToast();
  if (!version) return null;
  const verb = mode === "delete" ? "Delete" : "Unlist";
  return (
    <>
      <div className="row">
        <Button
          size="sm"
          variant="secondary"
          icon="eye-off"
          onClick={() => setMode("unlist")}
        >
          Unlist {version}
        </Button>
        <Button
          size="sm"
          variant="danger"
          icon="trash-2"
          onClick={() => setMode("delete")}
        >
          Delete {version}
        </Button>
      </div>
      {mode && (
        <Modal
          title={`${verb} version ${version}`}
          description={
            mode === "delete"
              ? "Removes this version, its assets and tags from the feed. Stored bytes are reclaimed later by garbage collection. This cannot be undone."
              : "Hides this version from listings. Its content is kept and the version can be listed again."
          }
          onClose={() => setMode("")}
          actions={
            <>
              <Button variant="secondary" onClick={() => setMode("")}>
                Cancel
              </Button>
              <Button
                variant={mode === "delete" ? "danger" : "primary"}
                disabled={action.busy}
                onClick={async () => {
                  const ok = await action.run(async () => {
                    await client.call("deletePackageVersion", {
                      params: { id: feedId, component, version },
                      query: { action: mode },
                    });
                    toast(
                      mode === "delete"
                        ? "Version deleted"
                        : "Version unlisted",
                      version,
                    );
                    done();
                  });
                  void ok;
                }}
              >
                {verb} version
              </Button>
            </>
          }
        >
          <Failure error={action.error} />
        </Modal>
      )}
    </>
  );
}
