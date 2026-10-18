import { useState } from "react";
import { requireETag, type Client } from "./api";
import { isUnavailable, paths, type WebhookStats } from "./gaps";
import type { Webhook, WebhookDelivery } from "./generated";
import {
  Badge,
  Button,
  Drawer,
  EmptyState,
  Input,
  Modal,
  PropertyList,
  SegmentedControl,
  Switch,
  Table,
} from "./ds/bundle";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  Needs,
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

function checkSecret(s: string) {
  const n = new TextEncoder().encode(s).length;
  if (n < 32 || n > 4096)
    throw Error(
      "Signing secret must contain 32–4096 UTF-8 bytes. Enter it again to submit.",
    );
}
const secretHint =
  "32–4096 UTF-8 bytes. Cleared on every submission; never returned by the service.";

export function Webhooks({ client }: { client: Client }) {
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
          onClick={r.reload}
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
                  key: "endpoint",
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
                      {w.endpoint}
                      <small>{w.id}</small>
                    </button>
                  ),
                },
                {
                  key: "ok",
                  label: "Success 7 d",
                  align: "right",
                  render: (w: Webhook) => {
                    const v = (w as WebhookStats & Webhook).success_rate_7d;
                    return v == null ? "—" : `${v.toFixed(1)}%`;
                  },
                },
                {
                  key: "created_at",
                  label: "Created",
                  render: (w: Webhook) => when(w.created_at),
                },
                { key: "revision", label: "Revision", align: "right" },
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
      <Needs block>success-rate aggregate, redeliver</Needs>
      {creating && (
        <CreateWebhook
          client={client}
          onClose={() => setCreating(false)}
          done={() => {
            setCreating(false);
            r.reload();
          }}
        />
      )}
      {current && (
        <WebhookDrawer
          key={`${current.id}:${current.revision}`}
          client={client}
          item={current}
          onClose={() => setOpen(undefined)}
          reload={r.reload}
        />
      )}
    </>
  );
}

function CreateWebhook({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [endpoint, setEndpoint] = useState("");
  const [secret, setSecret] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Create webhook"
      description="No URL credentials, query, or fragment. The endpoint must be a public HTTPS URL."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form="create-webhook" type="submit" disabled={action.busy}>
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
          const submitted = secret;
          setSecret("");
          const ok = await action.run(async () => {
            checkSecret(submitted);
            await client.call("createWebhook", {
              body: { endpoint, secret: submitted },
            });
          });
          if (ok) {
            toast("Webhook created", endpoint);
            done();
          }
        }}
      >
        <Input
          label="Webhook endpoint"
          type="url"
          mono
          required
          maxLength={2048}
          value={endpoint}
          onChange={val(setEndpoint)}
        />
        <Input
          label="Signing secret"
          type="password"
          mono
          required
          autoComplete="new-password"
          maxLength={4096}
          value={secret}
          onChange={val(setSecret)}
          hint={secretHint}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function WebhookDrawer({
  client,
  item,
  onClose,
  reload,
}: {
  client: Client;
  item: Webhook;
  onClose: () => void;
  reload: () => void;
}) {
  const [enabled, setEnabled] = useState(item.enabled);
  const [rotating, setRotating] = useState(false);
  const save = useAction();
  const toast = useToast();
  const etag = requireETag(`"${item.revision}"`);
  return (
    <Drawer
      open
      title={item.endpoint}
      description={<Copyable text={item.id} />}
      onClose={onClose}
      width={640}
      footer={
        <>
          <Button
            variant="secondary"
            icon="rotate-ccw"
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
              body: { enabled },
            }),
          );
          if (ok) {
            toast("Webhook state saved", enabled ? "Enabled" : "Disabled");
            reload();
          }
        }}
      >
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
            disabled={save.busy || enabled === item.enabled}
          >
            {save.busy ? "Saving…" : "Save webhook state"}
          </Button>
        </div>
        <Failure error={save.error} />
      </form>
      <Deliveries client={client} id={item.id} />
      {rotating && (
        <RotateSecret
          client={client}
          item={item}
          etag={etag}
          onClose={() => setRotating(false)}
          done={() => {
            setRotating(false);
            reload();
          }}
        />
      )}
    </Drawer>
  );
}

function RotateSecret({
  client,
  item,
  etag,
  onClose,
  done,
}: {
  client: Client;
  item: Webhook;
  etag: string;
  onClose: () => void;
  done: () => void;
}) {
  const [secret, setSecret] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Rotate signing secret"
      description="The new secret replaces the current one for all future deliveries."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form="rotate-secret" type="submit" disabled={action.busy}>
            {action.busy ? "Rotating…" : "Rotate signing secret"}
          </Button>
        </>
      }
    >
      <form
        id="rotate-secret"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const submitted = secret;
          setSecret("");
          const ok = await action.run(async () => {
            checkSecret(submitted);
            await client.call("rotateWebhookSecret", {
              params: { id: item.id },
              etag,
              body: { secret: submitted },
            });
          });
          if (ok) {
            toast("Signing secret rotated");
            done();
          }
        }}
      >
        <Input
          label="Signing secret"
          type="password"
          mono
          required
          autoComplete="new-password"
          maxLength={4096}
          value={secret}
          onChange={val(setSecret)}
          hint={secretHint}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
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
                              await client.extra(
                                "POST",
                                paths.redeliver(id, d.event_id),
                                { body: {} },
                              );
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
