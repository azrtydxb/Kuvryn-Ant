import { useState } from "react";
import { Client, APIError, requireETag } from "./api";
import type {
  Source,
  RemoteSourceConfig,
  RemoteSourceRecord,
} from "./generated";
import { Field, Failure, Mutation, Reload, useResource } from "./ui";
const initial: RemoteSourceConfig = {
  origin: "",
  redirect_origins: [],
  allow_private: false,
  timeout_seconds: 30,
  cache: {
    metadata_ttl_seconds: 300,
    negative_ttl_seconds: 30,
    stale_if_error_seconds: 0,
    max_entries: 1000,
    max_bytes: 67108864,
  },
};
export function RemoteSource({
  client,
  source,
  close,
}: {
  client: Client;
  source: Source;
  close: () => void;
}) {
  const r = useResource(
    () => client.call("getRemoteSourceConfig", { params: { id: source.id } }),
    [client, source.id],
  );
  const base = useResource(
    () => client.call("getSource", { params: { id: source.id } }),
    [client, source.id],
  );
  const missing = r.error instanceof APIError && r.error.status === 404;
  return (
    <section className="card">
      <div className="heading">
        <h2>Remote source: {source.name}</h2>
        <button onClick={close}>Close remote configuration</button>
      </div>
      <Reload
        loading={r.loading || base.loading}
        reload={() => {
          r.reload();
          base.reload();
        }}
      />
      {!missing && <Failure error={r.error} />}
      <Failure error={base.error} />
      {r.loading && <p role="status">Loading remote configuration…</p>}
      {missing && (
        <p>
          No accessible remote configuration was found. Configure access is
          required; a new configuration can be submitted using the current
          source revision.
        </p>
      )}
      {(r.value || (missing && base.value)) && (
        <RemoteEditor
          key={r.value?.etag ?? base.value?.etag}
          client={client}
          source={source.id}
          record={r.value?.data}
          etag={r.value?.etag ?? base.value?.etag ?? null}
          reload={() => {
            r.reload();
            base.reload();
          }}
        />
      )}
    </section>
  );
}
function RemoteEditor({
  client,
  source,
  record,
  etag,
  reload,
}: {
  client: Client;
  source: string;
  record?: RemoteSourceRecord;
  etag: string | null;
  reload: () => void;
}) {
  const [config, setConfig] = useState<RemoteSourceConfig>(
    record?.config ?? initial,
  );
  const [redirects, setRedirects] = useState(
    config.redirect_origins.join("\n"),
  );
  const [authorization, setAuthorization] = useState("");
  const [apiKey, setApiKey] = useState("");
  return (
    <>
      <Mutation
        label="Save remote configuration"
        run={async () => {
          const origins = redirects
            .split(/\r?\n/)
            .map((s) => s.trim())
            .filter(Boolean);
          if (origins.length > 16)
            throw Error("At most 16 redirect origins are supported.");
          await client.call("setRemoteSourceConfig", {
            params: { id: source },
            etag: requireETag(etag),
            body: { ...config, redirect_origins: origins },
          });
          reload();
        }}
      >
        <Field
          label="Upstream HTTPS base URL"
          hint="No credentials, query, fragment, or dot path segments."
        >
          <input
            required
            type="url"
            pattern="https://.*"
            maxLength={4096}
            value={config.origin}
            onChange={(e) => setConfig({ ...config, origin: e.target.value })}
          />
        </Field>
        <Field
          label="Allowed redirect origins"
          hint="One HTTPS origin per line, at most 16. Empty denies cross-origin redirects."
        >
          <textarea
            value={redirects}
            onChange={(e) => setRedirects(e.target.value)}
          />
        </Field>
        <Field
          label="Operator CA ID"
          hint="An ID configured by the operator, never a filesystem path. Empty uses system trust."
        >
          <input
            pattern="[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}"
            maxLength={64}
            value={config.ca_ref ?? ""}
            onChange={(e) => setConfig({ ...config, ca_ref: e.target.value })}
          />
        </Field>
        <label className="check">
          <input
            type="checkbox"
            checked={config.allow_private}
            onChange={(e) =>
              setConfig({ ...config, allow_private: e.target.checked })
            }
          />
          Allow private upstream addresses
        </label>
        <Field label="Request timeout (seconds)">
          <input
            type="number"
            min={1}
            max={300}
            required
            value={config.timeout_seconds}
            onChange={(e) =>
              setConfig({ ...config, timeout_seconds: Number(e.target.value) })
            }
          />
        </Field>
        <h3>Cache bounds</h3>
        <div className="grid">
          {(
            [
              ["metadata_ttl_seconds", "Metadata TTL (seconds)", 1, 86400],
              ["negative_ttl_seconds", "Negative TTL (seconds)", 1, 300],
              ["stale_if_error_seconds", "Stale on error (seconds)", 0, 86400],
              ["max_entries", "Maximum entries", 1, 100000],
              ["max_bytes", "Maximum bytes", 1024, 1073741824],
            ] as const
          ).map(([key, label, min, max]) => (
            <Field label={label} key={key}>
              <input
                type="number"
                required
                min={min}
                max={max}
                value={config.cache[key]}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    cache: { ...config.cache, [key]: Number(e.target.value) },
                  })
                }
              />
            </Field>
          ))}
        </div>
        {config.credential_id && (
          <>
            <p>
              Attached credential reference: <code>{config.credential_id}</code>
            </p>
            <button
              type="button"
              onClick={() => setConfig({ ...config, credential_id: "" })}
            >
              Detach credential on save
            </button>
          </>
        )}
        <p>
          Saving configuration does not verify upstream connectivity or protocol
          support.
        </p>
      </Mutation>
      {record && (
        <details>
          <summary>Set or replace upstream credentials</summary>
          <Mutation
            label="Save upstream credentials"
            run={async () => {
              const body = { authorization, api_key: apiKey };
              setAuthorization("");
              setApiKey("");
              if (!body.authorization.trim() && !body.api_key.trim())
                throw Error("Enter at least one credential value.");
              await client.call("setRemoteSourceCredential", {
                params: { id: source },
                etag: requireETag(etag),
                body,
              });
              reload();
            }}
          >
            <Field
              label="Authorization header"
              hint="Complete header value, for example Bearer followed by the upstream token."
            >
              <input
                type="password"
                autoComplete="new-password"
                maxLength={8192}
                pattern="[ -~]*"
                value={authorization}
                onChange={(e) => setAuthorization(e.target.value)}
              />
            </Field>
            <Field label="X-Api-Key header">
              <input
                type="password"
                autoComplete="new-password"
                maxLength={8192}
                pattern="[ -~]*"
                value={apiKey}
                onChange={(e) => setApiKey(e.target.value)}
              />
            </Field>
            <p>
              Values are cleared on submission, stored encrypted per source, and
              never returned. Re-enter after a failed request; refresh the
              revision before changing an uncertain result.
            </p>
          </Mutation>
        </details>
      )}
    </>
  );
}
