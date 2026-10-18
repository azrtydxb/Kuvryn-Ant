import { useState } from "react";
import { Client, requireETag } from "./api";
import type { Webhook } from "./generated";
import { Field, Failure, Mutation, Reload, useResource } from "./ui";
import { Pages } from "./Packages";
export function Webhooks({ client }: { client: Client }) {
  const [after, setAfter] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [secret, setSecret] = useState("");
  const r = useResource(
    () =>
      client.call("listWebhooks", {
        query: { limit: "25", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  return (
    <>
      <div className="heading">
        <h1>Webhooks</h1>
        <Reload {...r} />
      </div>
      <p>
        Signed event delivery to public HTTPS endpoints. Configuration does not
        test connectivity.
      </p>
      <details className="card">
        <summary>Create webhook</summary>
        <Mutation
          label="Create webhook"
          run={async () => {
            const submitted = secret;
            setSecret("");
            checkSecret(submitted);
            await client.call("createWebhook", {
              body: { endpoint, secret: submitted },
            });
            setEndpoint("");
            r.reload();
          }}
        >
          <Field label="Webhook endpoint">
            <input
              type="url"
              required
              maxLength={2048}
              value={endpoint}
              onChange={(e) => setEndpoint(e.target.value)}
            />
          </Field>
          <p>No URL credentials, query, or fragment.</p>
          <Secret value={secret} set={setSecret} />
        </Mutation>
      </details>
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading webhooks…</p>}
      {r.value && (
        <>
          {r.value.data.items.map((w) => (
            <WebhookEditor
              key={`${w.id}:${w.revision}`}
              client={client}
              item={w}
              reload={r.reload}
            />
          ))}
          {!r.value.data.items.length && <p>No webhooks.</p>}
          <Pages cursor={after} next={r.value.data.next_after} set={setAfter} />
        </>
      )}
    </>
  );
}
function checkSecret(s: string) {
  const n = new TextEncoder().encode(s).length;
  if (n < 32 || n > 4096)
    throw Error(
      "Signing secret must contain 32–4096 UTF-8 bytes. Enter it again to submit.",
    );
}
function Secret({ value, set }: { value: string; set: (s: string) => void }) {
  return (
    <Field
      label="Signing secret"
      hint="32–4096 UTF-8 bytes. Cleared on every submission; never returned by the service."
    >
      <input
        type="password"
        autoComplete="new-password"
        required
        maxLength={4096}
        value={value}
        onChange={(e) => set(e.target.value)}
      />
    </Field>
  );
}
function WebhookEditor({
  client,
  item,
  reload,
}: {
  client: Client;
  item: Webhook;
  reload: () => void;
}) {
  const [secret, setSecret] = useState("");
  const [enabled, setEnabled] = useState(item.enabled);
  const [deliveries, setDeliveries] = useState(false);
  const etag = requireETag(`"${item.revision}"`);
  return (
    <section className="card">
      <h2>{item.endpoint}</h2>
      <code>{item.id}</code>
      <p>
        Revision {item.revision} · {item.enabled ? "Enabled" : "Disabled"}
      </p>
      <Mutation
        label="Save webhook state"
        run={async () => {
          await client.call("patchWebhook", {
            params: { id: item.id },
            etag,
            body: { enabled },
          });
          reload();
        }}
      >
        <label className="check">
          <input
            type="checkbox"
            checked={enabled}
            onChange={(e) => setEnabled(e.target.checked)}
          />
          Enable delivery
        </label>
      </Mutation>
      <details>
        <summary>Rotate signing secret</summary>
        <Mutation
          label="Rotate signing secret"
          run={async () => {
            const submitted = secret;
            setSecret("");
            checkSecret(submitted);
            await client.call("rotateWebhookSecret", {
              params: { id: item.id },
              etag,
              body: { secret: submitted },
            });
            reload();
          }}
        >
          <Secret value={secret} set={setSecret} />
        </Mutation>
      </details>
      <button onClick={() => setDeliveries((v) => !v)}>
        {deliveries ? "Hide deliveries" : "View deliveries"}
      </button>
      {deliveries && <Deliveries client={client} id={item.id} />}
    </section>
  );
}
function Deliveries({ client, id }: { client: Client; id: string }) {
  const [after, setAfter] = useState("");
  const [state, setState] = useState("");
  const [failures, setFailures] = useState(false);
  const r = useResource(
    () =>
      client.call(failures ? "listWebhookFailures" : "listWebhookDeliveries", {
        params: { id },
        query: {
          limit: "25",
          ...(after ? { after } : {}),
          ...(state ? { state } : {}),
        },
      }),
    [client, id, after, state, failures],
  );
  return (
    <>
      <h3>Delivery history</h3>
      <Field label="Delivery state">
        <select
          value={state}
          onChange={(e) => {
            setState(e.target.value);
            setAfter("");
          }}
        >
          <option value="">All states</option>
          {["pending", "delivered", "dead"].map((v) => (
            <option key={v}>{v}</option>
          ))}
        </select>
      </Field>
      <label className="check">
        <input
          type="checkbox"
          checked={failures}
          onChange={(e) => {
            setFailures(e.target.checked);
            setAfter("");
          }}
        />
        Failures only
      </label>
      <Reload {...r} />
      <Failure error={r.error} />
      {r.loading && <p role="status">Loading deliveries…</p>}
      {r.value && (
        <>
          <ul>
            {r.value.data.items.map((d) => (
              <li key={d.event_id}>
                Event {d.event_id} · {d.state} · Attempts {d.attempts}
                <p>
                  Available {d.available_at}
                  {d.delivered_at && ` · Delivered ${d.delivered_at}`}
                </p>
                {d.last_failure && <p>{d.last_failure}</p>}
              </li>
            ))}
          </ul>
          {!r.value.data.items.length && <p>No matching deliveries.</p>}
          <Pages
            cursor={after}
            next={r.value.data.next_after?.toString()}
            set={setAfter}
          />
        </>
      )}
    </>
  );
}
