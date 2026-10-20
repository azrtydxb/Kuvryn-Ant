import { useState } from "react";
import { requireETag, type Client } from "./api";
import { isUnavailable, useOptionalCall } from "./gaps";
import type { Go } from "./App";
import type { Webhook, WebhookDelivery, WebhookSecret } from "./generated";
import {
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

const NEW_SECRET = "__new__";

/** A display label: 1–100 printable characters, no surrounding space. */
export function nameError(name: string) {
  if (!name) return undefined;
  if (name !== name.trim()) return "No leading or trailing spaces.";
  // eslint-disable-next-line no-control-regex
  if (name.length > 100 || /[\u0000-\u001f\u007f]/.test(name))
    return "1–100 printable characters.";
  return undefined;
}

/** https URL without credentials, query or fragment (the server also checks egress rules). */
export function endpointError(url: string) {
  if (!url) return undefined;
  let u: URL;
  try {
    u = new URL(url);
  } catch {
    return "Enter a full URL such as https://hooks.example.com/ant.";
  }
  if (u.protocol !== "https:") return "Use https.";
  if (u.username || u.password) return "No credentials in the URL.";
  if (u.search || u.hash) return "No query string or fragment.";
  return undefined;
}

function useSecrets(client: Client) {
  return useResource(
    () => client.call("listWebhookSecrets", { query: { limit: "200" } }),
    [client],
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
        text="Signed event delivery to public HTTPS endpoints. Webhooks are configured here only; configuration does not test connectivity."
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
                      {w.name}
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
  secrets,
  reloadSecrets,
  onClose,
  done,
}: {
  client: Client;
  secrets: WebhookSecret[];
  reloadSecrets: () => void;
  onClose: () => void;
  done: () => void;
}) {
  const [name, setName] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [secretId, setSecretId] = useState("");
  const action = useAction();
  const toast = useToast();
  const valid =
    !!name.trim() &&
    !nameError(name) &&
    !!endpoint &&
    !endpointError(endpoint) &&
    !!secretId;
  return (
    <Modal
      title="Create webhook"
      description="Deliver signed events to a public HTTPS URL. The name is only a label."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-webhook"
            type="submit"
            disabled={action.busy || !valid}
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
              body: { name, endpoint, secret_id: secretId },
            }),
          );
          if (ok) {
            toast("Webhook created", name);
            done();
          }
        }}
      >
        <Input
          label="Name"
          required
          maxLength={100}
          value={name}
          onChange={val(setName)}
          error={nameError(name)}
          hint="A label for this webhook, unique on this instance."
        />
        <Input
          label="Webhook URL"
          type="url"
          mono
          required
          maxLength={2048}
          value={endpoint}
          onChange={val(setEndpoint)}
          error={endpointError(endpoint)}
          hint="Public HTTPS URL without credentials, query or fragment."
        />
        <SecretSelect
          client={client}
          secrets={secrets}
          value={secretId}
          onChange={setSecretId}
          reload={reloadSecrets}
        />
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
  const [enabled, setEnabled] = useState(item.enabled);
  const [name, setName] = useState(item.name);
  const [endpoint, setEndpoint] = useState(item.endpoint);
  const [secretId, setSecretId] = useState(item.secret_id ?? "");
  const [rotating, setRotating] = useState(false);
  const save = useAction();
  const toast = useToast();
  const etag = requireETag(`"${item.revision}"`);
  const secret = secrets.find((s) => s.id === item.secret_id);
  const nameChanged = name !== item.name;
  const endpointChanged = endpoint !== item.endpoint;
  const secretChanged = secretId !== (item.secret_id ?? "");
  const dirty =
    enabled !== item.enabled || nameChanged || endpointChanged || secretChanged;
  const bad =
    !name.trim() || !!nameError(name) || !endpoint || !!endpointError(endpoint);
  return (
    <Drawer
      open
      title={item.name}
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
                ...(nameChanged ? { name } : {}),
                ...(endpointChanged ? { endpoint } : {}),
                ...(secretChanged && secretId ? { secret_id: secretId } : {}),
              },
            }),
          );
          if (ok) {
            toast("Webhook saved", name);
            reload();
          }
        }}
      >
        <Input
          label="Name"
          maxLength={100}
          value={name}
          onChange={val(setName)}
          error={nameChanged ? nameError(name) : undefined}
        />
        <Input
          label="Webhook URL"
          type="url"
          mono
          maxLength={2048}
          value={endpoint}
          onChange={val(setEndpoint)}
          error={endpointChanged ? endpointError(endpoint) : undefined}
        />
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
            disabled={save.busy || !dirty || bad}
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
