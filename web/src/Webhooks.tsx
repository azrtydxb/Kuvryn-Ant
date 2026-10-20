import { useState } from "react";
import { requireETag, type Client } from "./api";
import { isUnavailable, useOptionalCall } from "./gaps";
import type { Go } from "./App";
import type { Webhook, WebhookDelivery, WebhookSecret } from "./generated";
import {
  Alert,
  Badge,
  Button,
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
import { SigningSecretModal } from "./WebhookSecrets";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  PageHeading,
  Pager,
  Unavailable,
  chk,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

const slugPattern = /^[a-z0-9-]{1,63}$/;
const NEW_SECRET = "__new__";

/** base + slug with exactly one slash between them; "" when there is no base. */
export function previewURL(base: string, slug: string) {
  if (!base) return "";
  return `${base.replace(/\/+$/, "")}/${slug}`;
}
export function slugError(slug: string) {
  if (!slug) return undefined;
  return slugPattern.test(slug)
    ? undefined
    : "Lowercase letters, digits and hyphens only, 1–63 characters.";
}

/** Instance webhook base URL from the settings endpoint ("" when unset). */
function useBaseURL(client: Client) {
  const s = useOptionalCall(client, "getSettings");
  if (s.state === "loading") return { loading: true, base: "" };
  if (s.state !== "ready") return { loading: false, base: "" };
  return {
    loading: false,
    base: s.data.items.find((i) => i.key === "webhook_base_url")?.value ?? "",
  };
}

function useSecrets(client: Client) {
  return useResource(
    () => client.call("listWebhookSecrets", { query: { limit: "200" } }),
    [client],
  );
}

/** URL the slug resolves to, copyable, with the validation message beneath. */
export function UrlPreview({ base, slug }: { base: string; slug: string }) {
  const error = slugError(slug);
  const url = previewURL(base, slug || "<name>");
  return (
    <div className="stack" data-testid="url-preview">
      <span className="muted small">Delivery URL</span>
      <div className="row spread">
        <Mono>{url}</Mono>
        {slug && !error && <Copyable text={url} label="Copy delivery URL" />}
      </div>
      {error && (
        <small role="alert" className="bad">
          {error}
        </small>
      )}
    </div>
  );
}

/** Signing secret picker; its last option opens the shared create modal. */
function SecretSelect({
  client,
  secrets,
  value,
  onChange,
  reload,
}: {
  client: Client;
  secrets: WebhookSecret[];
  value: string;
  onChange: (id: string) => void;
  reload: () => void;
}) {
  const [creating, setCreating] = useState(false);
  return (
    <>
      <Select
        label="Signing secret"
        options={[
          { value: "", label: "Select a signing secret…" },
          ...secrets.map((s) => ({ value: s.id, label: s.name })),
          { value: NEW_SECRET, label: "+ Create new signing secret…" },
        ]}
        value={value}
        onChange={(e: { target: { value: string } }) => {
          if (e.target.value === NEW_SECRET) setCreating(true);
          else onChange(e.target.value);
        }}
      />
      {creating && (
        <SigningSecretModal
          client={client}
          onSaved={(s) => {
            reload();
            onChange(s.id);
          }}
          onClose={() => setCreating(false)}
        />
      )}
    </>
  );
}

export function Webhooks({ client, go }: { client: Client; go?: Go }) {
  const [after, setAfter] = useState("");
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string>();
  const r = useResource(
    () =>
      client.call("listWebhooks", {
        query: { limit: "25", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  const secrets = useSecrets(client);
  const stats = useOptionalCall(client, "getWebhookStats", {
    query: { limit: "200" },
  });
  const rates = new Map(
    (stats.state === "ready" ? stats.data.items : []).flatMap((x) =>
      x.success_rate == null ? [] : [[x.webhook_id, x.success_rate] as const],
    ),
  );
  const current = r.value?.data.items.find((w) => w.id === open);
  return (
    <>
      <PageHeading
        eyebrow="Integrations"
        title="Webhooks"
        text="Signed event delivery to public HTTPS endpoints. Configuration does not test connectivity."
      >
        <Button
          variant="secondary"
          icon="refresh-cw"
          disabled={r.loading}
          onClick={() => {
            r.reload();
            secrets.reload();
          }}
        >
          Refresh
        </Button>
        <Button icon="plus" onClick={() => setCreating(true)}>
          Create webhook
        </Button>
      </PageHeading>
      <Failure error={r.error} />
      {r.loading && <Loading what="webhooks" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              onRowClick={(w: Webhook) => setOpen(w.id)}
              columns={[
                {
                  key: "name",
                  label: "Webhook",
                  render: (w: Webhook) => (
                    <button
                      type="button"
                      className="rowlink"
                      onClick={(e) => {
                        e.stopPropagation();
                        setOpen(w.id);
                      }}
                    >
                      {w.name || "Custom endpoint"}
                      {w.custom_endpoint && w.name && (
                        <small>custom endpoint</small>
                      )}
                    </button>
                  ),
                },
                {
                  key: "endpoint",
                  label: "URL",
                  render: (w: Webhook) => <Mono>{w.endpoint}</Mono>,
                },
                {
                  key: "secret",
                  label: "Signing secret",
                  render: (w: Webhook) =>
                    w.secret_name ? (
                      <button
                        type="button"
                        className="rowlink"
                        onClick={(e) => {
                          e.stopPropagation();
                          go?.("identity", { tab: "secrets" });
                        }}
                      >
                        {w.secret_name}
                      </button>
                    ) : (
                      "—"
                    ),
                },
                {
                  key: "ok",
                  label: "Success 7 d",
                  align: "right",
                  render: (w: Webhook) => {
                    const v = rates.get(w.id);
                    return v == null ? "—" : `${(v * 100).toFixed(1)}%`;
                  },
                },
                {
                  key: "enabled",
                  label: "Status",
                  render: (w: Webhook) => (
                    <Badge tone={w.enabled ? "good" : "neutral"} dot>
                      {w.enabled ? "Enabled" : "Disabled"}
                    </Badge>
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
            title="No webhooks"
            description="Create one to receive signed callbacks for artifact and configuration events."
            action={
              <Button size="sm" icon="plus" onClick={() => setCreating(true)}>
                Create webhook
              </Button>
            }
          />
        ))}
      {creating && (
        <CreateWebhook
          client={client}
          go={go}
          secrets={secrets.value?.data.items ?? []}
          reloadSecrets={secrets.reload}
          onClose={() => setCreating(false)}
          done={() => {
            setCreating(false);
            r.reload();
            secrets.reload();
          }}
        />
      )}
      {current && (
        <WebhookDrawer
          key={`${current.id}:${current.revision}`}
          client={client}
          go={go}
          item={current}
          secrets={secrets.value?.data.items ?? []}
          reloadSecrets={secrets.reload}
          onClose={() => setOpen(undefined)}
          reload={() => {
            r.reload();
            secrets.reload();
          }}
        />
      )}
    </>
  );
}

function CreateWebhook({
  client,
  go,
  secrets,
  reloadSecrets,
  onClose,
  done,
}: {
  client: Client;
  go?: Go;
  secrets: WebhookSecret[];
  reloadSecrets: () => void;
  onClose: () => void;
  done: () => void;
}) {
  const { base, loading } = useBaseURL(client);
  const [custom, setCustom] = useState(false);
  const [slug, setSlug] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [secretId, setSecretId] = useState("");
  const action = useAction();
  const toast = useToast();
  // Without a base URL only the custom endpoint (administrator) mode can work.
  const slugMode = !!base && !custom;
  const valid = slugMode
    ? slugPattern.test(slug) && !!secretId
    : !!endpoint && !!secretId;
  return (
    <Modal
      title="Create webhook"
      description="Named webhooks deliver to the instance webhook base URL plus the name, signed with a shared signing secret."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-webhook"
            type="submit"
            disabled={action.busy || loading || !valid}
          >
            {action.busy ? "Saving…" : "Create webhook"}
          </Button>
        </>
      }
    >
      <form
        id="create-webhook"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("createWebhook", {
              body: slugMode
                ? { name: slug, secret_id: secretId }
                : { endpoint, secret_id: secretId },
            }),
          );
          if (ok) {
            toast(
              "Webhook created",
              slugMode ? previewURL(base, slug) : endpoint,
            );
            done();
          }
        }}
      >
        {!loading && !base && (
          <Alert tone="info" title="No webhook base URL configured">
            Named webhooks need a base URL such as
            https://hooks.example.com/ant/.{" "}
            <Button
              size="sm"
              variant="ghost"
              type="button"
              onClick={() => go?.("settings")}
            >
              Open Settings
            </Button>{" "}
            Until then you can use a custom endpoint.
          </Alert>
        )}
        {slugMode ? (
          <>
            <Input
              label="Name"
              required
              mono
              maxLength={63}
              value={slug}
              onChange={val(setSlug)}
              error={slugError(slug)}
              hint="Lowercase letters, digits and hyphens."
            />
            <UrlPreview base={base} slug={slug} />
          </>
        ) : (
          <Input
            label="Webhook endpoint"
            type="url"
            mono
            required
            maxLength={2048}
            value={endpoint}
            onChange={val(setEndpoint)}
            hint="Custom endpoint: a public HTTPS URL without credentials, query or fragment."
          />
        )}
        <SecretSelect
          client={client}
          secrets={secrets}
          value={secretId}
          onChange={setSecretId}
          reload={reloadSecrets}
        />
        {!!base && (
          <Switch
            label="Use a custom endpoint instead"
            labelPosition="end"
            checked={custom}
            onChange={chk(setCustom)}
          />
        )}
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function WebhookDrawer({
  client,
  go,
  item,
  secrets,
  reloadSecrets,
  onClose,
  reload,
}: {
  client: Client;
  go?: Go;
  item: Webhook;
  secrets: WebhookSecret[];
  reloadSecrets: () => void;
  onClose: () => void;
  reload: () => void;
}) {
  const { base } = useBaseURL(client);
  const [enabled, setEnabled] = useState(item.enabled);
  const [slug, setSlug] = useState(item.name ?? "");
  const [secretId, setSecretId] = useState(item.secret_id ?? "");
  const [rotating, setRotating] = useState(false);
  const save = useAction();
  const toast = useToast();
  const etag = requireETag(`"${item.revision}"`);
  const secret = secrets.find((s) => s.id === item.secret_id);
  const slugChanged = !item.custom_endpoint && slug !== (item.name ?? "");
  const secretChanged = secretId !== (item.secret_id ?? "");
  const dirty = enabled !== item.enabled || slugChanged || secretChanged;
  const slugBad = slugChanged && !slugPattern.test(slug);
  return (
    <Drawer
      open
      title={item.name || item.endpoint}
      description={<Copyable text={item.id} />}
      onClose={onClose}
      width={640}
      footer={
        <>
          <Button
            variant="secondary"
            icon="rotate-ccw"
            disabled={!secret}
            onClick={() => setRotating(true)}
          >
            Rotate secret
          </Button>
          <Button onClick={onClose}>Done</Button>
        </>
      }
    >
      <PropertyList
        items={[
          {
            label: "Delivery URL",
            value: <Copyable text={item.endpoint} />,
          },
          {
            label: "Signing secret",
            value: item.secret_name ? (
              <button
                type="button"
                className="rowlink"
                onClick={() => go?.("identity", { tab: "secrets" })}
              >
                {item.secret_name}
              </button>
            ) : (
              "Inline (legacy)"
            ),
          },
          {
            label: "Endpoint",
            value: item.custom_endpoint ? "Custom endpoint" : "Named",
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
      <form
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await save.run(() =>
            client.call("patchWebhook", {
              params: { id: item.id },
              etag,
              body: {
                ...(enabled !== item.enabled ? { enabled } : {}),
                ...(slugChanged ? { name: slug } : {}),
                ...(secretChanged && secretId ? { secret_id: secretId } : {}),
              },
            }),
          );
          if (ok) {
            toast("Webhook saved", item.name || item.endpoint);
            reload();
          }
        }}
      >
        {!item.custom_endpoint && (
          <>
            <Input
              label="Name"
              mono
              maxLength={63}
              value={slug}
              onChange={val(setSlug)}
              error={slugChanged ? slugError(slug) : undefined}
            />
            {slugChanged && !slugBad && base && (
              <UrlPreview base={base} slug={slug} />
            )}
          </>
        )}
        <SecretSelect
          client={client}
          secrets={secrets}
          value={secretId}
          onChange={setSecretId}
          reload={reloadSecrets}
        />
        <div className="row spread">
          <Switch
            label="Enable delivery"
            labelPosition="end"
            checked={enabled}
            onChange={chk(setEnabled)}
          />
          <Button
            type="submit"
            size="sm"
            variant="secondary"
            disabled={save.busy || !dirty || slugBad}
          >
            {save.busy ? "Saving…" : "Save webhook"}
          </Button>
        </div>
        <Failure error={save.error} />
      </form>
      <Deliveries client={client} id={item.id} />
      {rotating && secret && (
        <SigningSecretModal
          client={client}
          secret={secret}
          exceptWebhook={item.id}
          onClose={() => {
            setRotating(false);
            reload();
          }}
        />
      )}
    </Drawer>
  );
}

const stateTone = {
  delivered: "good",
  pending: "info",
  dead: "bad",
} as const;

function Deliveries({ client, id }: { client: Client; id: string }) {
  const [after, setAfter] = useState("");
  const [state, setState] = useState("all");
  const [failures, setFailures] = useState(false);
  const [unavailable, setUnavailable] = useState(false);
  const action = useAction();
  const toast = useToast();
  const r = useResource(
    () =>
      client.call(failures ? "listWebhookFailures" : "listWebhookDeliveries", {
        params: { id },
        query: {
          limit: "25",
          ...(after ? { after } : {}),
          ...(state !== "all" ? { state } : {}),
        },
      }),
    [client, id, after, state, failures],
  );
  return (
    <>
      <h3 className="h4">Delivery history</h3>
      <div className="row wrap">
        <SegmentedControl
          aria-label="Delivery state"
          options={[
            { value: "all", label: "All" },
            { value: "pending", label: "Pending" },
            { value: "delivered", label: "Delivered" },
            { value: "dead", label: "Dead" },
          ]}
          value={state}
          onChange={(v: string) => {
            setState(v);
            setAfter("");
          }}
        />
        <Switch
          label="Failures only"
          labelPosition="end"
          checked={failures}
          onChange={chk((v) => {
            setFailures(v);
            setAfter("");
          })}
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
      {unavailable && <Unavailable what="Webhook redeliver" compact />}
      {r.loading && <Loading what="deliveries" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              rowKey="event_id"
              columns={[
                {
                  key: "event_id",
                  label: "Event",
                  mono: true,
                },
                {
                  key: "state",
                  label: "State",
                  render: (d: WebhookDelivery) => (
                    <Badge tone={stateTone[d.state]} dot>
                      {d.state}
                    </Badge>
                  ),
                },
                { key: "attempts", label: "Attempts", align: "right" },
                {
                  key: "when",
                  label: "Time",
                  render: (d: WebhookDelivery) => (
                    <>
                      {when(d.delivered_at ?? d.available_at)}
                      {d.last_failure && (
                        <small>
                          <Mono>{d.last_failure}</Mono>
                        </small>
                      )}
                    </>
                  ),
                },
                {
                  key: "x",
                  label: "",
                  align: "right",
                  render: (d: WebhookDelivery) =>
                    d.state === "dead" ? (
                      <Button
                        size="sm"
                        variant="ghost"
                        disabled={action.busy}
                        onClick={async () => {
                          setUnavailable(false);
                          let gone = false;
                          const ok = await action.run(async () => {
                            try {
                              await client.call("redeliverWebhookDelivery", {
                                params: { id, event_id: String(d.event_id) },
                              });
                            } catch (e) {
                              if (!isUnavailable(e)) throw e;
                              gone = true;
                              setUnavailable(true);
                            }
                          });
                          if (ok && !gone) {
                            toast("Redelivery queued", `Event ${d.event_id}`);
                            r.reload();
                          }
                        }}
                      >
                        Redeliver
                      </Button>
                    ) : null,
                },
              ]}
              rows={r.value.data.items}
            />
            <Pager
              cursor={after}
              next={r.value.data.next_after?.toString()}
              set={setAfter}
            />
          </>
        ) : (
          <p className="muted">No matching deliveries.</p>
        ))}
    </>
  );
}
