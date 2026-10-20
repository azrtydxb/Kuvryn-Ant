import { useState, type FormEvent } from "react";
import { APIError, requireETag, type Client } from "./api";
import type {
  RemoteSourceConfig,
  RemoteSourceRecord,
  Source,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  SettingsRow,
  SettingsSection,
  Switch,
  Textarea,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  chk,
  useAction,
  useResource,
  useToast,
  val,
} from "./ui";

export const initial: RemoteSourceConfig = {
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
const cacheFields = [
  ["metadata_ttl_seconds", "Metadata TTL (seconds)", 1, 86400],
  ["negative_ttl_seconds", "Negative TTL (seconds)", 1, 300],
  ["stale_if_error_seconds", "Stale on error (seconds)", 0, 86400],
  ["max_entries", "Maximum entries", 1, 100000],
  ["max_bytes", "Maximum bytes", 1024, 1073741824],
] as const;

export function RemoteSource({
  client,
  source,
  onSaved,
}: {
  client: Client;
  source: Source;
  onSaved?: () => void;
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
  const reload = () => {
    r.reload();
    base.reload();
    onSaved?.();
  };
  return (
    <>
      {!missing && <Failure error={r.error} />}
      <Failure error={base.error} />
      {(r.loading || base.loading) && <Loading what="remote configuration" />}
      {missing && (
        <Alert tone="info" title="No remote configuration yet">
          No accessible remote configuration was found. Submit one using the
          current source revision. Configure access is required.
        </Alert>
      )}
      {(r.value || (missing && base.value)) && (
        <RemoteEditor
          key={r.value?.etag ?? base.value?.etag}
          client={client}
          source={source.id}
          record={r.value?.data}
          etag={r.value?.etag ?? base.value?.etag ?? null}
          reload={reload}
        />
      )}
    </>
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
  const save = useAction();
  const creds = useAction();
  const toast = useToast();
  async function saveConfig(e: FormEvent) {
    e.preventDefault();
    const ok = await save.run(async () => {
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
    });
    if (ok) {
      toast("Remote configuration saved");
      reload();
    }
  }
  async function saveCredentials(e: FormEvent) {
    e.preventDefault();
    const body = { authorization, api_key: apiKey };
    // Secrets never stay in state after submission, even when it fails.
    setAuthorization("");
    setApiKey("");
    const ok = await creds.run(async () => {
      if (!body.authorization.trim() && !body.api_key.trim())
        throw Error("Enter at least one credential value.");
      await client.call("setRemoteSourceCredential", {
        params: { id: source },
        etag: requireETag(etag),
        body,
      });
    });
    if (ok) {
      toast(
        "Upstream credentials saved",
        "Stored encrypted; never shown again",
      );
      reload();
    }
  }
  return (
    <>
      <form onSubmit={saveConfig}>
        <SettingsSection
          title="Upstream"
          form
          footer={
            <Button type="submit" disabled={save.busy}>
              {save.busy ? "Saving…" : "Save remote configuration"}
            </Button>
          }
          footerNote="Saving does not verify upstream connectivity or protocol support."
        >
          <Input
            label="Upstream HTTPS base URL"
            mono
            required
            type="url"
            pattern="https://.*"
            maxLength={4096}
            value={config.origin}
            onChange={val((origin) => setConfig({ ...config, origin }))}
            hint="No credentials, query, fragment, or dot path segments."
          />
          <Textarea
            label="Allowed redirect origins"
            mono
            rows={3}
            value={redirects}
            onChange={val(setRedirects)}
            hint="One HTTPS origin per line, at most 16. Empty denies cross-origin redirects."
          />
          <div className="cols">
            <Input
              label="Request timeout (seconds)"
              type="number"
              mono
              required
              min={1}
              max={300}
              value={config.timeout_seconds}
              onChange={val((v) =>
                setConfig({ ...config, timeout_seconds: Number(v) }),
              )}
            />
            <Input
              label="Operator CA ID"
              mono
              pattern="[A-Za-z0-9][A-Za-z0-9_.\-]{0,63}"
              maxLength={64}
              placeholder="System trust store"
              value={config.ca_ref ?? ""}
              onChange={val((ca_ref) => setConfig({ ...config, ca_ref }))}
              hint="An ID configured by the operator, never a path."
            />
          </div>
          <SettingsRow
            label="Allow private upstream addresses"
            description="Permits origins that resolve to private or loopback ranges."
          >
            <Switch
              aria-label="Allow private upstream addresses"
              checked={config.allow_private}
              onChange={chk((allow_private) =>
                setConfig({ ...config, allow_private }),
              )}
            />
          </SettingsRow>
          <h4 className="h4">Cache bounds</h4>
          <div className="cols">
            {cacheFields.map(([key, label, min, max]) => (
              <Input
                key={key}
                label={label}
                type="number"
                mono
                required
                min={min}
                max={max}
                value={config.cache[key]}
                onChange={val((v) =>
                  setConfig({
                    ...config,
                    cache: { ...config.cache, [key]: Number(v) },
                  }),
                )}
              />
            ))}
            <Input
              label="Largest in-memory metadata document (bytes, 0 = default)"
              type="number"
              mono
              min={0}
              max={1073741824}
              value={config.cache.max_metadata_bytes ?? 0}
              onChange={val((v) =>
                setConfig({
                  ...config,
                  cache: { ...config.cache, max_metadata_bytes: Number(v) },
                }),
              )}
            />
          </div>
          {config.credential_id && (
            <SettingsRow
              label="Attached credential reference"
              description="Stored encrypted. Never shown again after saving."
            >
              <Badge tone="good" icon="key-round">
                Set · <Mono>{config.credential_id}</Mono>
              </Badge>
              <Button
                type="button"
                size="sm"
                variant="secondary"
                onClick={() => setConfig({ ...config, credential_id: "" })}
              >
                Detach credential on save
              </Button>
            </SettingsRow>
          )}
          <Failure error={save.error} />
        </SettingsSection>
      </form>
      {record && (
        <form onSubmit={saveCredentials} autoComplete="off">
          <SettingsSection
            title="Upstream credentials"
            description="Set or replace. Values are cleared on submission, stored encrypted per source, and never returned. Re-enter after a failed request; refresh the revision before changing an uncertain result."
            form
            footer={
              <Button type="submit" variant="secondary" disabled={creds.busy}>
                {creds.busy ? "Saving…" : "Save upstream credentials"}
              </Button>
            }
          >
            <Input
              label="Authorization header"
              mono
              type="password"
              autoComplete="new-password"
              maxLength={8192}
              pattern="[ -~]*"
              value={authorization}
              onChange={val(setAuthorization)}
              hint="Complete header value, for example Bearer followed by the upstream token."
            />
            <Input
              label="X-Api-Key header"
              mono
              type="password"
              autoComplete="new-password"
              maxLength={8192}
              pattern="[ -~]*"
              value={apiKey}
              onChange={val(setApiKey)}
            />
            <Failure error={creds.error} />
          </SettingsSection>
        </form>
      )}
    </>
  );
}
