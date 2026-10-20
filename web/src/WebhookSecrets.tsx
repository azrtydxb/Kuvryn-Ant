import { useState } from "react";
import { requireETag, type Client } from "./api";
import type { WebhookSecret } from "./generated";
import {
  Alert,
  Button,
  EmptyState,
  Input,
  Modal,
  Select,
  Table,
  Textarea,
} from "./ds/bundle";
import {
  Copyable,
  Failure,
  Loading,
  Mono,
  Pager,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

const graceOptions = [
  { value: "0", label: "No grace window" },
  { value: "3600", label: "1 hour" },
  { value: "86400", label: "24 hours" },
  { value: "604800", label: "7 days" },
];

export function usedByText(s: WebhookSecret, except?: string) {
  const others = s.used_by.filter((w) => w.id !== except);
  return others.map((w) => w.name || w.endpoint).join(", ");
}

/**
 * One modal for creating and rotating a signing secret, used under Access and
 * from the webhook form. The value is shown exactly once after success; the
 * secret is reported through onSaved as soon as it exists so a caller can
 * select it while the value is still on screen.
 */
export function SigningSecretModal({
  client,
  secret,
  exceptWebhook,
  onClose,
  onSaved,
}: {
  client: Client;
  /** When set the modal rotates this secret instead of creating one. */
  secret?: WebhookSecret;
  exceptWebhook?: string;
  onClose: () => void;
  onSaved?: (s: WebhookSecret) => void;
}) {
  const rotating = !!secret;
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [mode, setMode] = useState("generate");
  const [value, setValue] = useState("");
  const [grace, setGrace] = useState("0");
  const [result, setResult] = useState<WebhookSecret>();
  const action = useAction();
  const toast = useToast();
  const others = secret ? usedByText(secret, exceptWebhook) : "";
  const title = rotating ? "Rotate signing secret" : "Create signing secret";

  if (result)
    return (
      <Modal
        title={rotating ? "Signing secret rotated" : "Signing secret created"}
        onClose={onClose}
        actions={<Button onClick={onClose}>I have stored it</Button>}
      >
        <div className="stack">
          <Alert tone="warn" title="Store it now">
            This value is shown only once. The service keeps it encrypted and
            cannot show it again.
          </Alert>
          <div className="stack">
            <span className="muted small">Signing secret value</span>
            <div className="row spread" data-testid="secret-value">
              <Mono>{result.value ?? "(not available on replay)"}</Mono>
              {result.value && (
                <Copyable text={result.value} label="Copy signing secret" />
              )}
            </div>
          </div>
          {result.previous_valid_until && (
            <p className="muted small">
              The previous value also signs until{" "}
              {when(result.previous_valid_until)}; X-Ant-Signature carries both
              signatures until then.
            </p>
          )}
        </div>
      </Modal>
    );

  return (
    <Modal
      title={title}
      description={
        rotating
          ? `Replaces the value of ${secret.name} for every webhook that uses it.`
          : "A named HMAC key that webhooks reference. Generated or pasted, it is encrypted at rest and never returned again."
      }
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form="signing-secret" type="submit" disabled={action.busy}>
            {action.busy
              ? "Saving…"
              : rotating
                ? "Rotate secret"
                : "Create secret"}
          </Button>
        </>
      }
    >
      <form
        id="signing-secret"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          // This modal can sit inside another form; React bubbles portal events.
          e.stopPropagation();
          const pasted = mode === "paste" ? value : "";
          setValue("");
          const ok = await action.run(async () => {
            if (mode === "paste") {
              const n = new TextEncoder().encode(pasted).length;
              if (n < 32 || n > 4096)
                throw Error(
                  "Signing secret must contain 32–4096 UTF-8 bytes. Enter it again to submit.",
                );
            }
            const body = pasted ? { value: pasted } : {};
            const saved = secret
              ? (
                  await client.call("rotateWebhookSecretValue", {
                    params: { id: secret.id },
                    etag: requireETag(`"${secret.revision}"`),
                    body: { ...body, grace_seconds: Number(grace) },
                  })
                ).data
              : (
                  await client.call("createWebhookSecret", {
                    body: {
                      name,
                      ...(description ? { description } : {}),
                      ...body,
                    },
                  })
                ).data;
            setResult(saved);
            onSaved?.(saved);
          });
          if (ok)
            toast(
              rotating ? "Signing secret rotated" : "Signing secret created",
            );
        }}
      >
        {rotating && others && (
          <Alert tone="warn" title="Shared secret">
            Also used by: {others}. Their deliveries switch to the new value
            immediately; update every receiver.
          </Alert>
        )}
        {!rotating && (
          <>
            <Input
              label="Name"
              required
              maxLength={200}
              value={name}
              onChange={val(setName)}
              hint="Unique per instance, for example production-receivers."
            />
            <Textarea
              label="Description"
              maxLength={1000}
              value={description}
              onChange={val(setDescription)}
            />
          </>
        )}
        <Select
          label="Value"
          options={[
            { value: "generate", label: "Generate 32 random bytes" },
            { value: "paste", label: "Paste my own value" },
          ]}
          value={mode}
          onChange={val(setMode)}
        />
        {mode === "paste" && (
          <Input
            label="Secret value"
            type="password"
            mono
            required
            autoComplete="new-password"
            maxLength={4096}
            value={value}
            onChange={val(setValue)}
            hint="32–4096 UTF-8 bytes. Cleared on every submission; never returned by the service."
          />
        )}
        {rotating && (
          <Select
            label="Keep the previous value valid"
            options={graceOptions}
            value={grace}
            onChange={val(setGrace)}
          />
        )}
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

/** Access → Signing secrets: list, create, rotate and delete. */
export function SigningSecretsTab({
  client,
  creating,
  setCreating,
}: {
  client: Client;
  creating: boolean;
  setCreating: (v: boolean) => void;
}) {
  const [after, setAfter] = useState("");
  const [rotating, setRotating] = useState<WebhookSecret>();
  const [deleting, setDeleting] = useState<WebhookSecret>();
  const r = useResource(
    () =>
      client.call("listWebhookSecrets", {
        query: { limit: "50", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  return (
    <>
      <p className="muted small">
        Named HMAC keys that webhooks reference. Values are encrypted and never
        shown again after creation.
      </p>
      <Failure error={r.error} />
      {r.loading && <Loading what="signing secrets" />}
      {r.value &&
        (r.value.data.items.length ? (
          <>
            <Table
              columns={[
                {
                  key: "name",
                  label: "Name",
                  render: (s: WebhookSecret) => (
                    <>
                      {s.name}
                      {s.description && <small>{s.description}</small>}
                    </>
                  ),
                },
                {
                  key: "used",
                  label: "Used by",
                  align: "right",
                  render: (s: WebhookSecret) => (
                    <span title={usedByText(s)}>{s.used_by_count}</span>
                  ),
                },
                {
                  key: "created_at",
                  label: "Created",
                  render: (s: WebhookSecret) => when(s.created_at),
                },
                {
                  key: "rotated_at",
                  label: "Rotated",
                  render: (s: WebhookSecret) =>
                    s.rotated_at ? when(s.rotated_at) : "—",
                },
                {
                  key: "x",
                  label: "",
                  align: "right",
                  render: (s: WebhookSecret) => (
                    <span className="row tight">
                      <Button
                        size="sm"
                        variant="secondary"
                        icon="rotate-ccw"
                        onClick={() => setRotating(s)}
                      >
                        Rotate
                      </Button>
                      <Button
                        size="sm"
                        variant="ghost"
                        icon="trash-2"
                        aria-label={`Delete ${s.name}`}
                        onClick={() => setDeleting(s)}
                      >
                        Delete
                      </Button>
                    </span>
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
            icon="key-round"
            title="No signing secrets"
            description="Create one, then select it when you create a webhook."
            action={
              <Button size="sm" icon="plus" onClick={() => setCreating(true)}>
                Create signing secret
              </Button>
            }
          />
        ))}
      {creating && (
        <SigningSecretModal
          client={client}
          onClose={() => {
            setCreating(false);
            r.reload();
          }}
        />
      )}
      {rotating && (
        <SigningSecretModal
          client={client}
          secret={rotating}
          onClose={() => {
            setRotating(undefined);
            r.reload();
          }}
        />
      )}
      {deleting && (
        <DeleteSecret
          client={client}
          secret={deleting}
          onClose={() => setDeleting(undefined)}
          done={() => {
            setDeleting(undefined);
            r.reload();
          }}
        />
      )}
    </>
  );
}

function DeleteSecret({
  client,
  secret,
  onClose,
  done,
}: {
  client: Client;
  secret: WebhookSecret;
  onClose: () => void;
  done: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const inUse = secret.used_by_count > 0;
  return (
    <Modal
      title="Delete signing secret"
      description={`Delete ${secret.name}? Its value is destroyed and cannot be recovered.`}
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            disabled={action.busy || inUse}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("deleteWebhookSecret", {
                  params: { id: secret.id },
                  etag: requireETag(`"${secret.revision}"`),
                }),
              );
              if (ok) {
                toast("Signing secret deleted", secret.name);
                done();
              }
            }}
          >
            {action.busy ? "Deleting…" : "Delete secret"}
          </Button>
        </>
      }
    >
      {inUse && (
        <Alert tone="warn" title="Still in use">
          Used by {usedByText(secret)}. Move those webhooks to another secret
          first.
        </Alert>
      )}
      <Failure error={action.error} />
    </Modal>
  );
}
