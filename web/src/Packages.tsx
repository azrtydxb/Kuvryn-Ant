import { useState } from "react";
import { Client } from "./api";
import type { DiscoveredPackage, DiscoveredAsset } from "./generated";
import { Field, Failure, Reload, useResource } from "./ui";

export function Pages({
  cursor,
  next,
  set,
}: {
  cursor: string;
  next?: string;
  set: (v: string) => void;
}) {
  return (
    <div className="pager">
      <button disabled={!cursor} onClick={() => set("")}>
        First page
      </button>
      <button disabled={!next} onClick={() => set(next!)}>
        Next page
      </button>
    </div>
  );
}
export function Packages({ client, id }: { client: Client; id: string }) {
  const [cursor, setCursor] = useState("");
  const [prefix, setPrefix] = useState("");
  const [namespace, setNamespace] = useState("");
  const [digest, setDigest] = useState("");
  const [query, setQuery] = useState<Record<string, string>>({});
  const [selected, setSelected] = useState<DiscoveredPackage>();
  const r = useResource(
    () =>
      client.call("listPackages", {
        params: { id },
        query: { limit: "25", cursor, ...query },
      }),
    [client, id, cursor, query],
  );
  return (
    <section className="card">
      <div className="heading">
        <h2>Packages</h2>
        <Reload {...r} />
      </div>
      <p>
        Packages visible through this feed’s source order and your artifact
        permissions. Metadata only; shadowed sources do not fall through.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          setCursor("");
          setSelected(undefined);
          setQuery({
            ...(prefix ? { prefix } : {}),
            ...(namespace ? { namespace } : {}),
            ...(digest ? { digest } : {}),
          });
        }}
      >
        <div className="grid">
          <Field label="Package prefix">
            <input
              value={prefix}
              maxLength={200}
              onChange={(e) => setPrefix(e.target.value)}
            />
          </Field>
          <Field label="Exact namespace">
            <input
              value={namespace}
              maxLength={256}
              onChange={(e) => setNamespace(e.target.value)}
            />
          </Field>
          <Field
            label="SHA-256 checksum"
            hint="sha256: followed by 64 lowercase hexadecimal digits."
          >
            <input
              value={digest}
              pattern="sha256:[0-9a-f]{64}"
              onChange={(e) => setDigest(e.target.value)}
            />
          </Field>
        </div>
        <button>Search packages</button>
      </form>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading packages…</p>}
      {r.value && (
        <>
          <ul className="attachments">
            {r.value.data.items.map((p) => (
              <li key={p.id}>
                <button className="link" onClick={() => setSelected(p)}>
                  {p.coordinate}
                </button>
                <p>Namespace: {p.namespace || "(root)"}</p>
                <code>Source: {p.source_id}</code>
              </li>
            ))}
          </ul>
          {!r.value.data.items.length && (
            <p>No visible packages match these filters.</p>
          )}
          <Pages
            cursor={cursor}
            next={r.value.data.page.next_cursor}
            set={setCursor}
          />
        </>
      )}
      {selected && (
        <PackageDetail
          key={selected.id}
          client={client}
          id={id}
          item={selected}
          close={() => setSelected(undefined)}
        />
      )}
    </section>
  );
}
function PackageDetail({
  client,
  id,
  item,
  close,
}: {
  client: Client;
  id: string;
  item: DiscoveredPackage;
  close: () => void;
}) {
  const [cursor, setCursor] = useState("");
  const [tagCursor, setTagCursor] = useState("");
  const [version, setVersion] = useState("");
  const params = { id, component: item.id };
  const r = useResource(
    () =>
      client.call("listPackageVersions", {
        params,
        query: { limit: "25", cursor },
      }),
    [client, id, item.id, cursor],
  );
  const tags = useResource(
    () =>
      client.call("listPackageTags", {
        params,
        query: { limit: "25", cursor: tagCursor },
      }),
    [client, id, item.id, tagCursor],
  );
  return (
    <section>
      <h3>{item.coordinate}</h3>
      <button onClick={close}>Close package</button>
      <details>
        <summary>Package metadata</summary>
        <pre>{JSON.stringify(item.metadata, null, 2)}</pre>
      </details>
      <h4>Versions</h4>
      <Reload {...r} />
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading versions…</p>}
      {r.value && (
        <>
          <ul>
            {r.value.data.items.map((v) => (
              <li key={v.id}>
                <button className="link" onClick={() => setVersion(v.id)}>
                  {v.version}
                </button>{" "}
                · {v.created_at} · Revision {v.revision}
              </li>
            ))}
          </ul>
          {!r.value.data.items.length && <p>No visible versions.</p>}
          <Pages
            cursor={cursor}
            next={r.value.data.page.next_cursor}
            set={setCursor}
          />
        </>
      )}
      <h4>Tags</h4>
      <Reload {...tags} />
      <Failure error={tags.error} />
      {tags.loading && <p role="status">Loading tags…</p>}
      {tags.value && (
        <>
          <ul>
            {tags.value.data.items.map((t) => (
              <li key={t.name}>
                <button
                  className="link"
                  onClick={() => setVersion(t.version_id)}
                >
                  {t.name}
                </button>
                <code>{t.version_id}</code>
              </li>
            ))}
          </ul>
          {!tags.value.data.items.length && <p>No tags.</p>}
          <Pages
            cursor={tagCursor}
            next={tags.value.data.page.next_cursor}
            set={setTagCursor}
          />
        </>
      )}
      {version && (
        <Assets
          key={version}
          client={client}
          id={id}
          component={item.id}
          version={version}
        />
      )}
    </section>
  );
}
function Assets({
  client,
  id,
  component,
  version,
}: {
  client: Client;
  id: string;
  component: string;
  version: string;
}) {
  const [cursor, setCursor] = useState("");
  const [selected, setSelected] = useState("");
  const r = useResource(
    () =>
      client.call("listPackageAssets", {
        params: { id, component, version },
        query: { limit: "25", cursor },
      }),
    [client, id, component, version, cursor],
  );
  return (
    <section>
      <h4>Version assets</h4>
      <code>{version}</code>
      <Reload {...r} />
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading assets…</p>}
      {r.value && (
        <>
          <ul className="attachments">
            {r.value.data.items.map((a) => (
              <li key={a.id}>
                <button className="link" onClick={() => setSelected(a.id)}>
                  {a.key}
                </button>
                <code>{a.digest}</code>
                <span>
                  {a.size_bytes} bytes · {a.media_type}
                </span>
              </li>
            ))}
          </ul>
          {!r.value.data.items.length && <p>No visible assets.</p>}
          <Pages
            cursor={cursor}
            next={r.value.data.page.next_cursor}
            set={setCursor}
          />
        </>
      )}
      {selected && (
        <AssetDetail key={selected} client={client} id={id} asset={selected} />
      )}
    </section>
  );
}
function AssetDetail({
  client,
  id,
  asset,
}: {
  client: Client;
  id: string;
  asset: string;
}) {
  const r = useResource(
    () => client.call("getPackageAsset", { params: { id, asset } }),
    [client, id, asset],
  );
  return (
    <section>
      <h4>Asset provenance</h4>
      <Reload {...r} />
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading provenance…</p>}
      {r.value && <AssetRecord item={r.value.data} />}
    </section>
  );
}
export function AssetRecord({ item }: { item: DiscoveredAsset }) {
  return (
    <>
      <dl>
        {(
          [
            ["Key", item.key],
            ["Source", item.source_id],
            ["Checksum", item.digest],
            ["Size", `${item.size_bytes} bytes`],
            ["Content type", item.media_type],
            ["Created", item.created_at],
            ["Uploader", item.uploader_id || "Not recorded"],
          ] as const
        ).map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <details>
        <summary>Recorded provenance metadata</summary>
        <pre>{JSON.stringify(item.provenance, null, 2)}</pre>
      </details>
    </>
  );
}
