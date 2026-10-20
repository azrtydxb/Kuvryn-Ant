import { useState } from "react";
import { createPortal } from "react-dom";
import { requireETag, type Client } from "./api";
import { useAllFeeds, useAllSources } from "./data";
import { isUnavailable, useOptionalCall } from "./gaps";
import type { Go } from "./App";
import type {
  CreateIncomingHook,
  IncomingHook,
  IncomingHookAction,
  IncomingHookConfig,
  IncomingHookDelivery,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  Drawer,
  EmptyState,
  Input,
  Modal,
  PropertyList,
  SegmentedControl,
  Select,
  Switch,
  Table,
} from "./ds/bundle";
import { SecretSelect, useSecrets } from "./OutgoingWebhooks";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  Pager,
  Unavailable,
  bytes,
  chk,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

const slugPattern = /^[a-z0-9-]{1,63}$/;

/** Validation message for a hook slug; undefined while empty or valid. */
export function slugError(slug: string) {
  if (!slug) return undefined;
  return slugPattern.test(slug)
    ? undefined
    : "Lowercase letters, digits and hyphens only, 1–63 characters.";
}

/** The URL a hook is served at: base + slug, plus the secret suffix when known. */
export function hookURL(base: string, slug: string, token?: string) {
  if (!base) return "";
  return `${base.replace(/\/+$/, "")}/${slug}${token ? `/${token}` : ""}`;
}

const authLabels = {
  hmac: "HMAC signature",
  url_token: "Secret token in the URL",
  bearer: "Bearer Ant token",
} as const;
type Auth = keyof typeof authLabels;

const formatLabels: Record<string, string> = {
  github: "GitHub (X-Hub-Signature-256)",
  gitlab: "GitLab (X-Gitlab-Token)",
  ant: "Generic (X-Ant-Signature)",
  npm: "npm (X-NPM-Signature)",
  artifactory: "Artifactory (X-JFrog-Event-Auth)",
  nexus: "Nexus (X-Nexus-Webhook-Signature)",
};
const formatShort: Record<string, string> = {
  github: "GitHub",
  gitlab: "GitLab",
  ant: "generic",
  npm: "npm",
  artifactory: "Artifactory",
  nexus: "Nexus",
};

const actionLabels = {
  upstream_notify: "Upstream change notification",
  prewarm: "Prewarm a package or image",
  replication_run: "Run a replication job now",
  purge_source_cache: "Purge a remote source's cache",
  clean_cache: "Clean a cache feed by prefix",
  relay: "Relay to an internal URL",
} as const;
type ActionType = keyof typeof actionLabels;

const providerLabels: Record<string, string> = {
  auto: "Detect from the payload",
  dockerhub: "Docker Hub",
  github: "GitHub (release, package, registry_package)",
  gitlab: "GitLab (push, tag, release)",
  npm: "npm",
  artifactory: "Artifactory",
  nexus: "Nexus",
};

export function authText(h: IncomingHook) {
  if (h.auth_mode === "hmac")
    return `HMAC · ${formatShort[h.hmac_format ?? ""] ?? h.hmac_format ?? ""}`;
  if (h.auth_mode === "url_token") return "Secret in URL";
  return `Bearer · ${h.bearer_scope ?? ""}`;
}

/* ---------- form draft ---------- */

type Draft = {
  slug: string;
  description: string;
  enabled: boolean;
  auth: Auth;
  format: string;
  secretId: string;
  scope: string;
  rate: string;
  type: ActionType;
  feedId: string;
  kind: "package" | "image";
  name: string;
  version: string;
  jobId: string;
  sourceId: string;
  coordinate: string;
  prefix: string;
  provider: string;
  sourceIds: string[];
  origins: string;
  prewarm: boolean;
  targetURL: string;
  resignId: string;
};

function draftOf(h?: IncomingHook): Draft {
  const a = h?.action;
  return {
    slug: h?.slug ?? "",
    description: h?.description ?? "",
    enabled: h?.enabled ?? true,
    auth: (h?.auth_mode as Auth | undefined) ?? "hmac",
    format: h?.hmac_format ?? "github",
    secretId: h?.signing_secret_id ?? "",
    scope: h?.bearer_scope ?? "configure",
    rate: String(h?.rate_per_minute ?? 60),
    type: (a?.type as ActionType | undefined) ?? "upstream_notify",
    feedId: a?.feed_id ?? "",
    kind: a?.kind ?? "package",
    name: a?.name ?? "",
    version: a?.version ?? "",
    jobId: a?.job_id ?? "",
    sourceId: a?.source_id ?? "",
    coordinate: a?.coordinate ?? "",
    prefix: a?.prefix ?? "",
    provider: a?.provider ?? "auto",
    sourceIds: a?.source_ids ?? [],
    origins: (a?.origins ?? []).join(", "),
    prewarm: a?.prewarm ?? false,
    targetURL: a?.target_url ?? "",
    resignId: a?.resign_secret_id ?? "",
  };
}

function actionOf(d: Draft): IncomingHookAction {
  switch (d.type) {
    case "prewarm":
      return {
        type: "prewarm",
        feed_id: d.feedId,
        kind: d.kind,
        name: d.name,
        ...(d.version ? { version: d.version } : {}),
      };
    case "replication_run":
      return { type: "replication_run", job_id: d.jobId };
    case "purge_source_cache":
      return {
        type: "purge_source_cache",
        source_id: d.sourceId,
        ...(d.coordinate ? { coordinate: d.coordinate } : {}),
      };
    case "clean_cache":
      return { type: "clean_cache", feed_id: d.feedId, prefix: d.prefix };
    case "upstream_notify": {
      const origins = d.origins
        .split(/[\s,]+/)
        .map((o) => o.trim())
        .filter(Boolean);
      return {
        type: "upstream_notify",
        provider: d.provider as IncomingHookAction["provider"],
        ...(d.sourceIds.length ? { source_ids: d.sourceIds } : {}),
        ...(origins.length ? { origins } : {}),
        prewarm: d.prewarm,
        ...(d.prewarm && d.feedId ? { feed_id: d.feedId } : {}),
      };
    }
    case "relay":
      return {
        type: "relay",
        target_url: d.targetURL,
        ...(d.resignId ? { resign_secret_id: d.resignId } : {}),
      };
  }
}

/** The request body for a draft; the server validates it against the instance. */
export function bodyOf(d: Draft): CreateIncomingHook {
  return {
    slug: d.slug,
    description: d.description,
    enabled: d.enabled,
    auth_mode: d.auth,
    ...(d.auth === "hmac"
      ? {
          hmac_format: d.format as CreateIncomingHook["hmac_format"],
          signing_secret_id: d.secretId,
        }
      : {}),
    ...(d.auth === "bearer"
      ? { bearer_scope: d.scope as CreateIncomingHook["bearer_scope"] }
      : {}),
    rate_per_minute: Number(d.rate) || 60,
    action: actionOf(d),
  };
}

/** What is missing before the form can be submitted; undefined when complete. */
export function draftProblem(d: Draft): string | undefined {
  if (!slugPattern.test(d.slug)) return "A valid slug is required.";
  if (d.auth === "hmac" && !d.secretId) return "Choose a signing secret.";
  const rate = Number(d.rate);
  if (!Number.isInteger(rate) || rate < 1 || rate > 6000)
    return "Rate limit must be between 1 and 6000 per minute.";
  switch (d.type) {
    case "prewarm":
      if (!d.feedId || !d.name) return "Choose a feed and a name template.";
      break;
    case "replication_run":
      if (!d.jobId) return "Choose a replication job.";
      break;
    case "purge_source_cache":
      if (!d.sourceId) return "Choose a remote source.";
      break;
    case "clean_cache":
      if (!d.feedId || !d.prefix)
        return "Choose a cache feed and a prefix template.";
      break;
    case "upstream_notify":
      if (
        (d.provider === "artifactory" || d.provider === "nexus") &&
        !d.sourceIds.length &&
        !d.origins.trim()
      )
        return "Name the remote sources or origins this provider maps to.";
      break;
    case "relay":
      if (!/^https:\/\/[^\s/?#@]+/i.test(d.targetURL))
        return "Enter an https target URL.";
      break;
  }
  return undefined;
}

/* ---------- list ---------- */

export function IncomingHooks({
  client,
  go,
  slot,
}: {
  client: Client;
  go?: Go;
  slot: HTMLElement | null;
}) {
  const [after, setAfter] = useState("");
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string>();
  const [created, setCreated] = useState<IncomingHook>();
  const config = useOptionalCall(client, "getIncomingHookConfig");
  const cfg = config.state === "ready" ? config.data : undefined;
  const r = useResource(
    () =>
      client.call("listIncomingHooks", {
        query: { limit: "25", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  const current = r.value?.data.items.find((h) => h.id === open);
  if (r.error && isUnavailable(r.error))
    return <Unavailable what="Incoming webhooks" />;
  return (
    <>
      <p className="muted small">
        External systems call{" "}
        <Mono>{cfg?.hook_base_url || "<public URL>/hook/"}&lt;slug&gt;</Mono>.
        Each hook authenticates the caller and runs one action; every call is
        recorded in its delivery log.
      </p>
      {slot &&
        createPortal(
          <>
            <Button
              variant="secondary"
              icon="refresh-cw"
              disabled={r.loading}
              onClick={r.reload}
            >
              Refresh
            </Button>
            <Button icon="plus" onClick={() => setCreating(true)}>
              Create incoming hook
            </Button>
          </>,
          slot,
        )}
      {cfg && !cfg.public_url && (
        <Alert tone="warn" title="No public URL configured">
          Set ANT_PUBLIC_URL so hook URLs can be shown in full.
        </Alert>
      )}
      <Failure error={r.error} />
      {r.loading && <Loading what="incoming hooks" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              onRowClick={(h: IncomingHook) => setOpen(h.id)}
              columns={[
                {
                  key: "slug",
                  label: "Hook",
                  render: (h: IncomingHook) => (
                    <div className="hook-cell">
                      <button
                        type="button"
                        className="rowlink"
                        onClick={(e) => {
                          e.stopPropagation();
                          setOpen(h.id);
                        }}
                      >
                        {h.slug}
                      </button>
                      {!h.enabled && (
                        <Badge tone="neutral" dot>
                          Disabled
                        </Badge>
                      )}
                      {h.description && (
                        <small className="muted">{h.description}</small>
                      )}
                      <Copyable
                        text={h.url || h.path}
                        label={`Copy URL of ${h.slug}`}
                      />
                      {h.auth_mode === "url_token" && (
                        <small className="muted">
                          secret suffix …{h.token_hint}
                        </small>
                      )}
                      <small className="muted hook-meta">
                        {authText(h)} · {h.action_summary}
                      </small>
                    </div>
                  ),
                },
                {
                  key: "auth",
                  label: "Auth",
                  render: (h: IncomingHook) => authText(h),
                },
                {
                  key: "action",
                  label: "Action",
                  render: (h: IncomingHook) => (
                    <span className="clamp" title={h.action_summary}>
                      {h.action_summary}
                    </span>
                  ),
                },
                {
                  key: "last",
                  label: "Last received · success 7 d",
                  render: (h: IncomingHook) => (
                    <div className="hook-cell">
                      <span>
                        {h.last_received_at
                          ? when(h.last_received_at)
                          : "Never"}
                      </span>
                      <small className="muted">
                        {h.success_rate == null
                          ? "No finished deliveries"
                          : `${(h.success_rate * 100).toFixed(0)}% of ${h.succeeded_7d + h.failed_7d} succeeded`}
                      </small>
                    </div>
                  ),
                },
              ]}
              rows={r.value.data.items}
            />
            <Pager
              cursor={after}
              next={r.value.data.next_after}
              set={setAfter}
            />
          </>
        ) : (
          <EmptyState
            icon="webhook"
            title="No incoming hooks"
            description="Create one to let GitHub, GitLab, a registry or CI trigger prewarming, cache invalidation, replication or a relay."
            action={
              <Button size="sm" icon="plus" onClick={() => setCreating(true)}>
                Create incoming hook
              </Button>
            }
          />
        ))}
      {creating && cfg && (
        <HookModal
          client={client}
          go={go}
          config={cfg}
          onClose={() => setCreating(false)}
          done={(h) => {
            setCreating(false);
            r.reload();
            if (h.token) setCreated(h);
          }}
        />
      )}
      {created && cfg && (
        <TokenModal
          hook={created}
          config={cfg}
          onClose={() => setCreated(undefined)}
        />
      )}
      {current && cfg && (
        <HookDrawer
          key={`${current.id}:${current.revision}`}
          client={client}
          go={go}
          config={cfg}
          item={current}
          onClose={() => setOpen(undefined)}
          reload={r.reload}
          onToken={setCreated}
        />
      )}
    </>
  );
}

/* ---------- form ---------- */

function HookFields({
  client,
  config,
  draft,
  set,
  editing,
  initialSlug,
}: {
  client: Client;
  config: IncomingHookConfig;
  draft: Draft;
  set: <K extends keyof Draft>(key: K, value: Draft[K]) => void;
  editing: boolean;
  initialSlug?: string;
}) {
  const feeds = useAllFeeds(client);
  const sources = useAllSources(client);
  const secrets = useSecrets(client);
  const jobs = useOptionalCall(client, "listReplicationJobs");
  const remote = sources.items.filter((s) => s.kind === "remote");
  const cacheFeeds = feeds.items.filter((f) => f.purpose === "cache");
  const plainFeeds = feeds.items.filter((f) => f.purpose !== "cache");
  const jobItems = jobs.state === "ready" ? jobs.data.items : [];
  const preview = hookURL(config.hook_base_url, draft.slug || "<slug>");
  const tokenNote =
    draft.auth === "url_token" ? "/<secret token, shown once>" : "";
  const pick = (
    label: string,
    value: string,
    options: { value: string; label: string }[],
    onChange: (v: string) => void,
  ) => (
    <Select
      label={label}
      value={value}
      options={[{ value: "", label: `Select…` }, ...options]}
      onChange={(e: { target: { value: string } }) => onChange(e.target.value)}
    />
  );
  return (
    <>
      <Input
        label="Slug"
        required
        mono
        maxLength={63}
        value={draft.slug}
        onChange={val((v) => set("slug", v))}
        error={slugError(draft.slug)}
        hint="Lowercase letters, digits and hyphens. Unique on this instance."
      />
      <div
        className="stack"
        data-testid="url-preview"
        hidden={editing && draft.slug === initialSlug}
      >
        <span className="muted small">Hook URL</span>
        {config.hook_base_url ? (
          <span className="row tight">
            {draft.slug && !slugError(draft.slug) ? (
              <Copyable text={preview} label="Copy hook URL" />
            ) : (
              <Mono muted>{preview}</Mono>
            )}
            {tokenNote && <small className="muted">{tokenNote}</small>}
          </span>
        ) : (
          <Mono muted>(no public URL configured)</Mono>
        )}
      </div>
      <Input
        label="Description"
        maxLength={1000}
        value={draft.description}
        onChange={val((v) => set("description", v))}
      />
      <Select
        label="Authentication"
        value={draft.auth}
        options={(Object.keys(authLabels) as Auth[]).map((k) => ({
          value: k,
          label: authLabels[k],
        }))}
        onChange={(e: { target: { value: string } }) =>
          set("auth", e.target.value as Auth)
        }
      />
      {draft.auth === "hmac" && (
        <>
          <Select
            label="Signature format"
            value={draft.format}
            options={config.hmac_formats.map((f) => ({
              value: f.format,
              label: formatLabels[f.format] ?? `${f.format} (${f.header})`,
            }))}
            onChange={(e: { target: { value: string } }) =>
              set("format", e.target.value)
            }
          />
          <SecretSelect
            client={client}
            secrets={secrets.value?.data.items ?? []}
            value={draft.secretId}
            onChange={(id) => set("secretId", id)}
            reload={secrets.reload}
          />
          <p className="muted small">
            Docker Hub cannot sign requests: use a secret token in the URL for
            it.
          </p>
        </>
      )}
      {draft.auth === "url_token" && (
        <p className="muted small">
          A 256-bit random suffix is generated and shown once. Callers use{" "}
          <Mono>…/hook/&lt;slug&gt;/&lt;token&gt;</Mono>; only a digest is
          stored.
        </p>
      )}
      {draft.auth === "bearer" && (
        <Select
          label="Token scope"
          value={draft.scope}
          options={[
            { value: "configure", label: "configure" },
            { value: "replicate", label: "replicate" },
            { value: "admin", label: "admin" },
          ]}
          hint="The caller sends Authorization: Bearer <Ant token> and the token must hold this action on the instance."
          onChange={(e: { target: { value: string } }) =>
            set("scope", e.target.value)
          }
        />
      )}
      <Select
        label="Action"
        value={draft.type}
        options={(Object.keys(actionLabels) as ActionType[]).map((k) => ({
          value: k,
          label: actionLabels[k],
        }))}
        onChange={(e: { target: { value: string } }) =>
          set("type", e.target.value as ActionType)
        }
      />
      {draft.type === "upstream_notify" && (
        <>
          <Select
            label="Provider"
            value={draft.provider}
            options={Object.entries(providerLabels).map(([value, label]) => ({
              value,
              label,
            }))}
            onChange={(e: { target: { value: string } }) =>
              set("provider", e.target.value)
            }
          />
          <fieldset className="plain">
            <legend>Remote sources (optional)</legend>
            <p className="muted small">
              Without a selection, sources are matched by origin: Docker Hub,
              GitHub, GitLab and npm are recognised automatically. Artifactory
              and Nexus need a selection or origins.
            </p>
            <div className="row wrap">
              {remote.map((s) => (
                <Checkbox
                  key={s.id}
                  label={s.name}
                  description={s.origin}
                  checked={draft.sourceIds.includes(s.id)}
                  onChange={chk((on) =>
                    set(
                      "sourceIds",
                      on
                        ? [...draft.sourceIds, s.id]
                        : draft.sourceIds.filter((x) => x !== s.id),
                    ),
                  )}
                />
              ))}
              {!remote.length && (
                <span className="muted small">No remote sources yet.</span>
              )}
            </div>
          </fieldset>
          <Input
            label="Origins (optional)"
            mono
            value={draft.origins}
            onChange={val((v) => set("origins", v))}
            hint="Host names of the registries this hook covers, separated by commas."
          />
          <Switch
            label="Also prewarm the changed tag or version"
            labelPosition="end"
            checked={draft.prewarm}
            onChange={chk((v) => set("prewarm", v))}
          />
        </>
      )}
      {draft.type === "prewarm" && (
        <>
          {pick(
            "Feed",
            draft.feedId,
            plainFeeds.map((f) => ({
              value: f.id,
              label: `${f.slug} (${f.format})`,
            })),
            (v) => set("feedId", v),
          )}
          <Select
            label="What the name denotes"
            value={draft.kind}
            options={[
              { value: "package", label: "Package" },
              { value: "image", label: "Container image" },
            ]}
            onChange={(e: { target: { value: string } }) =>
              set("kind", e.target.value as "package" | "image")
            }
          />
          <Input
            label="Name template"
            mono
            required
            value={draft.name}
            onChange={val((v) => set("name", v))}
            hint="For example {{ .repository.full_name }}. Paths into the JSON payload; filters: lower, upper, trimv, base."
          />
          <Input
            label="Version or tag template"
            mono
            value={draft.version}
            onChange={val((v) => set("version", v))}
            hint="For example {{ .release.tag_name | trimv }}"
          />
        </>
      )}
      {draft.type === "replication_run" &&
        pick(
          "Replication job",
          draft.jobId,
          jobItems.map((j) => ({ value: j.id, label: j.name })),
          (v) => set("jobId", v),
        )}
      {draft.type === "purge_source_cache" && (
        <>
          {pick(
            "Remote source",
            draft.sourceId,
            remote.map((s) => ({ value: s.id, label: s.name })),
            (v) => set("sourceId", v),
          )}
          <Input
            label="Coordinate template (optional)"
            mono
            value={draft.coordinate}
            onChange={val((v) => set("coordinate", v))}
            hint="Empty purges the whole source cache. For example {{ .package.name }}."
          />
        </>
      )}
      {draft.type === "clean_cache" && (
        <>
          {pick(
            "Cache feed",
            draft.feedId,
            cacheFeeds.map((f) => ({
              value: f.id,
              label: `${f.slug} (${f.format})`,
            })),
            (v) => set("feedId", v),
          )}
          <Input
            label="Prefix template"
            mono
            required
            value={draft.prefix}
            onChange={val((v) => set("prefix", v))}
            hint="<repository>[:<tag prefix>] for an OCI cache, a path prefix for a raw cache."
          />
        </>
      )}
      {draft.type === "relay" && (
        <>
          <Input
            label="Target URL"
            type="url"
            mono
            required
            maxLength={2048}
            value={draft.targetURL}
            onChange={val((v) => set("targetURL", v))}
            hint="Public HTTPS URL. Delivery retries and the log work like outgoing webhooks. Credentials are never forwarded."
          />
          <Select
            label="Re-sign with"
            value={draft.resignId}
            options={[
              {
                value: "",
                label: "Do not re-sign (forward the original signature)",
              },
              ...(secrets.value?.data.items ?? []).map((s) => ({
                value: s.id,
                label: s.name,
              })),
            ]}
            onChange={(e: { target: { value: string } }) =>
              set("resignId", e.target.value)
            }
          />
        </>
      )}
      <Input
        label="Rate limit per minute"
        type="number"
        min={1}
        max={6000}
        value={draft.rate}
        onChange={val((v) => set("rate", v))}
        hint={`Bodies larger than ${bytes(config.max_body_bytes)} are refused.`}
      />
      {editing && (
        <Switch
          label="Enabled"
          labelPosition="end"
          checked={draft.enabled}
          onChange={chk((v) => set("enabled", v))}
        />
      )}
    </>
  );
}

function useDraft(initial?: IncomingHook) {
  const [draft, setDraft] = useState<Draft>(() => draftOf(initial));
  const set = <K extends keyof Draft>(key: K, value: Draft[K]) =>
    setDraft((d) => ({ ...d, [key]: value }));
  return { draft, set };
}

function HookModal({
  client,
  config,
  onClose,
  done,
}: {
  client: Client;
  go?: Go;
  config: IncomingHookConfig;
  onClose: () => void;
  done: (h: IncomingHook) => void;
}) {
  const { draft, set } = useDraft();
  const action = useAction();
  const toast = useToast();
  const problem = draftProblem(draft);
  return (
    <Modal
      title="Create incoming hook"
      description="An external system calls this URL; Ant authenticates the call and runs one action."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-incoming-hook"
            type="submit"
            disabled={action.busy || !!problem}
          >
            {action.busy ? "Saving…" : "Create hook"}
          </Button>
        </>
      }
    >
      <form
        id="create-incoming-hook"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          let made: IncomingHook | undefined;
          const ok = await action.run(async () => {
            made = (
              await client.call("createIncomingHook", { body: bodyOf(draft) })
            ).data;
          });
          if (ok && made) {
            toast("Incoming hook created", draft.slug);
            done(made);
          }
        }}
      >
        <HookFields
          client={client}
          config={config}
          draft={draft}
          set={set}
          editing={false}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

/** Shows the secret URL of a url_token hook exactly once. */
function TokenModal({
  hook,
  config,
  onClose,
}: {
  hook: IncomingHook;
  config: IncomingHookConfig;
  onClose: () => void;
}) {
  const url = hookURL(config.hook_base_url, hook.slug, hook.token);
  return (
    <Modal
      title="Hook URL created"
      onClose={onClose}
      actions={<Button onClick={onClose}>I have stored it</Button>}
    >
      <div className="stack">
        <Alert tone="warn" title="Store it now">
          The secret part of this URL is shown only once. Ant keeps a digest and
          cannot show it again; regenerate the token if it is lost.
        </Alert>
        <div className="stack">
          <span className="muted small">Secret hook URL</span>
          <div className="row spread" data-testid="hook-url">
            <Copyable text={url} label="Copy secret hook URL" />
          </div>
        </div>
      </div>
    </Modal>
  );
}

/* ---------- detail ---------- */

function HookDrawer({
  client,
  config,
  item,
  onClose,
  reload,
  onToken,
}: {
  client: Client;
  go?: Go;
  config: IncomingHookConfig;
  item: IncomingHook;
  onClose: () => void;
  reload: () => void;
  onToken: (h: IncomingHook) => void;
}) {
  const { draft, set } = useDraft(item);
  const save = useAction();
  const test = useAction();
  const regen = useAction();
  const remove = useAction();
  const toast = useToast();
  const [confirming, setConfirming] = useState(false);
  const [logEpoch, setLogEpoch] = useState(0);
  const etag = requireETag(`"${item.revision}"`);
  const problem = draftProblem(draft);
  const dirty =
    JSON.stringify(bodyOf(draft)) !== JSON.stringify(bodyOf(draftOf(item)));
  return (
    <Drawer
      open
      title={item.slug}
      description={<Copyable text={item.id} />}
      onClose={onClose}
      width={720}
      footer={
        <>
          <Button
            variant="secondary"
            icon="send"
            disabled={test.busy}
            onClick={async () => {
              const ok = await test.run(() =>
                client.call("testIncomingHook", { params: { id: item.id } }),
              );
              if (ok) {
                toast("Test queued", "A sample payload was sent to the hook.");
                setTimeout(() => setLogEpoch((n) => n + 1), 1200);
              }
            }}
          >
            Send test
          </Button>
          {item.auth_mode === "url_token" && (
            <Button
              variant="secondary"
              icon="rotate-ccw"
              disabled={regen.busy}
              onClick={async () => {
                let fresh: IncomingHook | undefined;
                const ok = await regen.run(async () => {
                  fresh = (
                    await client.call("regenerateIncomingHookToken", {
                      params: { id: item.id },
                      etag,
                    })
                  ).data;
                });
                if (ok && fresh) {
                  reload();
                  onToken(fresh);
                }
              }}
            >
              Regenerate token
            </Button>
          )}
          <Button
            variant="danger"
            icon="trash"
            onClick={() => setConfirming(true)}
          >
            Delete
          </Button>
          <Button onClick={onClose}>Done</Button>
        </>
      }
    >
      <PropertyList
        items={[
          {
            label: "Hook URL",
            value: item.url ? (
              <span className="row tight">
                <Copyable text={item.url} label="Copy hook URL" />
                {item.auth_mode === "url_token" && (
                  <small className="muted">+ secret …{item.token_hint}</small>
                )}
              </span>
            ) : (
              item.path
            ),
          },
          { label: "Authentication", value: authText(item) },
          { label: "Action", value: item.action_summary },
          {
            label: "Last 7 days",
            value: `${item.received_7d} received · ${item.succeeded_7d} succeeded · ${item.failed_7d} failed`,
          },
          { label: "Revision", value: item.revision, mono: true },
          { label: "Created", value: when(item.created_at) },
          {
            label: "Status",
            value: (
              <Badge tone={item.enabled ? "good" : "neutral"} dot>
                {item.enabled ? "Enabled" : "Disabled"}
              </Badge>
            ),
          },
        ]}
      />
      <Failure error={test.error ?? regen.error ?? remove.error} />
      <DeliveryLog
        client={client}
        hook={item}
        epoch={logEpoch}
        retention={config.log_retention_days}
      />
      <h3 className="h4">Settings</h3>
      <form
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          let updated: IncomingHook | undefined;
          const ok = await save.run(async () => {
            updated = (
              await client.call("patchIncomingHook", {
                params: { id: item.id },
                etag,
                body: bodyOf(draft),
              })
            ).data;
          });
          if (ok) {
            toast("Incoming hook saved", draft.slug);
            reload();
            if (updated?.token) onToken(updated);
          }
        }}
      >
        <HookFields
          client={client}
          config={config}
          draft={draft}
          set={set}
          editing
          initialSlug={item.slug}
        />
        <div className="row spread">
          <span className="muted small">{problem}</span>
          <Button
            type="submit"
            size="sm"
            variant="secondary"
            disabled={save.busy || !dirty || !!problem}
          >
            {save.busy ? "Saving…" : "Save hook"}
          </Button>
        </div>
        <Failure error={save.error} />
      </form>
      {confirming && (
        <Modal
          title="Delete incoming hook"
          description={`Deletes ${item.slug} and its delivery log. Callers get 404 from then on.`}
          onClose={() => setConfirming(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setConfirming(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={remove.busy}
                onClick={async () => {
                  const ok = await remove.run(() =>
                    client.call("deleteIncomingHook", {
                      params: { id: item.id },
                      etag,
                    }),
                  );
                  if (ok) {
                    toast("Incoming hook deleted", item.slug);
                    setConfirming(false);
                    reload();
                    onClose();
                  }
                }}
              >
                Delete hook
              </Button>
            </>
          }
        >
          <Failure error={remove.error} />
        </Modal>
      )}
    </Drawer>
  );
}

/* ---------- delivery log ---------- */

const stateTone = {
  succeeded: "good",
  pending: "info",
  running: "info",
  failed: "bad",
  rejected: "bad",
} as const;
const authTone = (a: IncomingHookDelivery["auth_result"]) =>
  a === "ok" ? "good" : a === "test" || a === "redelivery" ? "info" : "bad";

function DeliveryLog({
  client,
  hook,
  epoch,
  retention,
}: {
  client: Client;
  hook: IncomingHook;
  epoch: number;
  retention: number;
}) {
  const [before, setBefore] = useState("");
  const [state, setState] = useState("all");
  const [detail, setDetail] = useState<IncomingHookDelivery>();
  const action = useAction();
  const toast = useToast();
  const r = useResource(
    () =>
      client.call("listIncomingHookDeliveries", {
        params: { id: hook.id },
        query: {
          limit: "25",
          ...(before ? { before } : {}),
          ...(state !== "all" ? { state } : {}),
        },
      }),
    [client, hook.id, before, state, epoch],
  );
  const redeliver = async (d: IncomingHookDelivery) => {
    const ok = await action.run(() =>
      client.call("redeliverIncomingHookDelivery", {
        params: { id: hook.id, delivery_id: d.id },
      }),
    );
    if (ok) {
      toast("Redelivery queued", d.id.slice(0, 8));
      setTimeout(r.reload, 1200);
    }
  };
  return (
    <>
      <h3 className="h4">Delivery log</h3>
      <p className="muted small">
        Every call is recorded for {retention} days with a redacted, truncated
        payload preview.
      </p>
      <div className="row wrap">
        <SegmentedControl
          aria-label="Delivery state"
          options={[
            { value: "all", label: "All" },
            { value: "succeeded", label: "Succeeded" },
            { value: "failed", label: "Failed" },
            { value: "rejected", label: "Rejected" },
            { value: "pending", label: "Pending" },
          ]}
          value={state}
          onChange={(v: string) => {
            setState(v);
            setBefore("");
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
      </div>
      <Failure error={r.error ?? action.error} />
      {r.loading && <Loading what="deliveries" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              rowKey="id"
              onRowClick={(d: IncomingHookDelivery) => setDetail(d)}
              columns={[
                {
                  key: "received_at",
                  label: "Received",
                  render: (d: IncomingHookDelivery) => (
                    <>
                      {when(d.received_at)}
                      {d.source_ip && (
                        <small>
                          <Mono>{d.source_ip}</Mono>
                        </small>
                      )}
                      <small className="muted hook-meta">
                        {d.auth_result} · {d.state}
                        {d.action_result ? ` · ${d.action_result}` : ""}
                      </small>
                    </>
                  ),
                },
                {
                  key: "auth",
                  label: "Auth",
                  render: (d: IncomingHookDelivery) => (
                    <Badge tone={authTone(d.auth_result)} dot>
                      {d.auth_result}
                    </Badge>
                  ),
                },
                {
                  key: "state",
                  label: "Result",
                  render: (d: IncomingHookDelivery) => (
                    <>
                      <Badge tone={stateTone[d.state]} dot>
                        {d.state}
                      </Badge>
                      {d.action_result && (
                        <small>
                          <Mono>{d.action_result}</Mono>
                        </small>
                      )}
                    </>
                  ),
                },
                {
                  key: "size",
                  label: "Size",
                  align: "right",
                  render: (d: IncomingHookDelivery) => bytes(d.payload_size),
                },
                {
                  key: "x",
                  label: "",
                  align: "right",
                  render: (d: IncomingHookDelivery) =>
                    d.state === "rejected" ? null : (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={action.busy}
                        onClick={(e: { stopPropagation: () => void }) => {
                          e.stopPropagation();
                          void redeliver(d);
                        }}
                      >
                        Redeliver
                      </Button>
                    ),
                },
              ]}
              rows={r.value.data.items}
            />
            <Pager
              cursor={before}
              next={r.value.data.next_before}
              set={setBefore}
            />
          </>
        ) : (
          <p className="muted">No matching deliveries.</p>
        ))}
      {detail && (
        <Modal
          title="Delivery"
          description={<Copyable text={detail.id} />}
          onClose={() => setDetail(undefined)}
          actions={<Button onClick={() => setDetail(undefined)}>Done</Button>}
        >
          <div className="stack">
            <PropertyList
              items={[
                { label: "Received", value: when(detail.received_at) },
                { label: "Source", value: detail.source_ip || "—", mono: true },
                { label: "Authentication", value: detail.auth_result },
                { label: "State", value: detail.state },
                { label: "Attempts", value: detail.attempts, mono: true },
                {
                  label: "Result",
                  value: detail.action_result || "—",
                  mono: true,
                },
                { label: "Payload", value: bytes(detail.payload_size) },
              ]}
            />
            {detail.error && (
              <Alert tone="bad" title="Error">
                {detail.error}
              </Alert>
            )}
            {Object.keys(detail.detail).length > 0 && (
              <pre className="hook-preview" aria-label="Action detail">
                {JSON.stringify(detail.detail, null, 2)}
              </pre>
            )}
            <span className="muted small">
              Payload preview (secrets redacted, truncated)
            </span>
            <pre className="hook-preview" aria-label="Payload preview">
              {detail.preview || "(empty)"}
            </pre>
          </div>
        </Modal>
      )}
    </>
  );
}
