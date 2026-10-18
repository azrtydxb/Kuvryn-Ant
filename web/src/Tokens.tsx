import { useEffect, useState } from "react";
import type { Client } from "./api";
import { useAllFeeds } from "./data";
import { paths, useOptional, type TokenRecord } from "./gaps";
import type {
  IdentityPrincipal,
  IssueServiceToken,
  IssuedServiceToken,
} from "./generated";
import { Actions } from "./Grants";
import {
  Alert,
  Badge,
  Button,
  IconButton,
  Input,
  Modal,
  SegmentedControl,
  Select,
  Table,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Needs,
  useAction,
  useToast,
  uuidPattern,
  val,
  when,
} from "./ui";

const DAY = 86400000;
const durations: Record<string, number> = {
  "30 d": 30,
  "90 d": 90,
  "180 d": 180,
  "1 year": 365,
};

export function TokenIssuer({
  client,
  principals,
  initial,
  onClose,
}: {
  client: Client;
  principals: IdentityPrincipal[];
  initial?: string;
  onClose: () => void;
}) {
  const services = principals.filter(
    (p) => p.kind === "service" && !p.disabled,
  );
  const feeds = useAllFeeds(client);
  const [principal, setPrincipal] = useState(initial ?? services[0]?.id ?? "");
  const [scopes, setScopes] = useState<IssueServiceToken["scopes"]>([
    { actions: ["read"] },
  ]);
  const [duration, setDuration] = useState("90 d");
  const [issued, setIssued] = useState<IssuedServiceToken>();
  const [attempted, setAttempted] = useState(false);
  const [copied, setCopied] = useState(false);
  const action = useAction();
  const toast = useToast();
  // The secret is shown once and cleared after a minute or when leaving.
  useEffect(() => {
    if (!issued) return;
    const timer = setTimeout(() => setIssued(undefined), 60000);
    return () => clearTimeout(timer);
  }, [issued]);
  const setScope = (
    i: number,
    v: Partial<IssueServiceToken["scopes"][number]>,
  ) => setScopes(scopes.map((s, n) => (n === i ? { ...s, ...v } : s)));
  async function issue() {
    await action.run(async () => {
      if (!principal) throw Error("Choose a service principal.");
      if (scopes.some((s) => !s.actions.length))
        throw Error("Each token scope needs at least one action.");
      // One-shot: the secret cannot be replayed, so this is never retried.
      setAttempted(true);
      const expires_at = new Date(
        Date.now() + durations[duration] * DAY,
      ).toISOString();
      const result = await client.call("issueServiceToken", {
        params: { id: principal },
        body: { scopes, expires_at },
      });
      setIssued(result.data);
      toast("Token issued", "Copy the secret now; it is cleared in 60 s");
    });
  }
  return (
    <Modal
      title={issued ? "Token issued" : "Issue token"}
      description={
        issued
          ? "Copy it now. It is shown once and cleared in 60 s."
          : "Scopes intersect the principal's role grants. Issuance is never retried automatically."
      }
      onClose={onClose}
      width={620}
      actions={
        issued ? (
          <Button onClick={onClose}>Done</Button>
        ) : attempted && !action.busy ? (
          <>
            <Button variant="secondary" onClick={onClose}>
              Close
            </Button>
            <Button
              variant="secondary"
              onClick={() => {
                setAttempted(false);
                action.clear();
              }}
            >
              Start a separate token issuance
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button
              icon="key-round"
              disabled={action.busy || !principal}
              onClick={issue}
            >
              {action.busy ? "Issuing…" : "Issue token once"}
            </Button>
          </>
        )
      }
    >
      <Failure error={action.error} />
      {issued ? (
        <>
          <p>
            Token ID <Mono>{issued.id}</Mono> · expires{" "}
            {when(issued.expires_at)}
          </p>
          <div className="row end">
            <Input
              label="New token secret — copy now"
              type="password"
              mono
              autoComplete="off"
              readOnly
              value={issued.secret}
              onFocus={(e: { currentTarget: HTMLInputElement }) =>
                e.currentTarget.select()
              }
            />
            <IconButton
              icon={copied ? "check" : "copy"}
              label="Copy token secret"
              onClick={async () => {
                try {
                  await navigator.clipboard.writeText(issued.secret);
                  setCopied(true);
                } catch {
                  /* clipboard unavailable: select the field and copy */
                }
              }}
            />
          </div>
          <Alert tone="warn" title="Not retried automatically">
            If this window closes before you copy the token, issue a new one.
          </Alert>
          <Button
            size="sm"
            variant="secondary"
            onClick={() => setIssued(undefined)}
          >
            Clear displayed secret
          </Button>
        </>
      ) : attempted ? (
        <Alert tone="warn" title="An issuance request was attempted">
          Its secret cannot be replayed, even if the response was lost. Start a
          separate issuance if you did not receive the token.
        </Alert>
      ) : (
        <>
          <Select
            label="Service principal"
            options={
              services.length
                ? services.map((p) => ({ value: p.id, label: p.subject }))
                : [{ value: "", label: "No enabled service principals" }]
            }
            value={principal}
            onChange={val(setPrincipal)}
          />
          {scopes.map((s, i) => (
            <fieldset className="rulebox" key={i}>
              <legend>Token scope {i + 1}</legend>
              <div className="cols">
                <Select
                  label="Feed"
                  options={[
                    { value: "", label: "All feeds" },
                    ...feeds.items.map((f) => ({ value: f.id, label: f.slug })),
                  ]}
                  value={s.feed_id ?? ""}
                  onChange={val((v) =>
                    setScope(i, { feed_id: v || undefined }),
                  )}
                />
                <Input
                  label="Namespace (optional)"
                  mono
                  maxLength={200}
                  value={s.namespace ?? ""}
                  onChange={val((v) =>
                    setScope(i, { namespace: v || undefined }),
                  )}
                />
              </div>
              <Actions
                value={s.actions}
                onChange={(actions) => setScope(i, { actions })}
              />
              <Button
                type="button"
                size="sm"
                variant="ghost"
                icon="trash-2"
                disabled={scopes.length === 1}
                onClick={() => setScopes(scopes.filter((_, n) => n !== i))}
              >
                Remove scope {i + 1}
              </Button>
            </fieldset>
          ))}
          <Button
            type="button"
            size="sm"
            variant="secondary"
            icon="plus"
            disabled={scopes.length >= 64}
            onClick={() => setScopes([...scopes, { actions: ["read"] }])}
          >
            Add token scope
          </Button>
          <div>
            <span className="az-field__label">Expires in</span>
            <SegmentedControl
              block
              aria-label="Token lifetime"
              options={Object.keys(durations)}
              value={duration}
              onChange={setDuration}
            />
          </div>
          <p className="muted small">
            Empty scope fields apply broadly. Expiry is at most one year.
          </p>
        </>
      )}
    </Modal>
  );
}

export function RevokeById({ client }: { client: Client }) {
  const [id, setID] = useState("");
  const [confirm, setConfirm] = useState(false);
  const action = useAction();
  const toast = useToast();
  return (
    <form
      className="stack"
      onSubmit={async (e) => {
        e.preventDefault();
        const ok = await action.run(() =>
          client.call("revokeServiceToken", { params: { id } }),
        );
        if (ok) {
          toast("Token revoked", id);
          setID("");
          setConfirm(false);
        }
      }}
    >
      <p className="muted small">
        Token listing is not exposed by this server yet. Enter the token ID
        recorded at issuance to revoke it.
      </p>
      <Input
        label="Token ID to revoke"
        mono
        required
        pattern={uuidPattern}
        value={id}
        onChange={(e: { target: { value: string } }) => {
          setID(e.target.value);
          setConfirm(false);
        }}
      />
      <label className="az-check">
        <input
          className="az-check__input"
          type="checkbox"
          required
          checked={confirm}
          onChange={(e) => setConfirm(e.target.checked)}
        />
        <span className="az-check__box" />
        <span className="az-check__text">
          <span className="az-check__label">Revoke this token immediately</span>
        </span>
      </label>
      <div>
        <Button type="submit" variant="danger" disabled={action.busy}>
          {action.busy ? "Revoking…" : "Revoke token"}
        </Button>
      </div>
      <Failure error={action.error} />
    </form>
  );
}

export function TokensTab({
  client,
  principals,
}: {
  client: Client;
  principals: IdentityPrincipal[];
}) {
  const list = useOptional<{ items: TokenRecord[] }>(client, paths.tokens, {
    limit: "100",
  });
  const [revoking, setRevoking] = useState<TokenRecord>();
  const action = useAction();
  const toast = useToast();
  const name = (id?: string) =>
    principals.find((p) => p.id === id)?.subject ?? id ?? "—";
  if (list.state === "loading") return <Loading what="tokens" />;
  if (list.state === "error") return <Failure error={list.error} />;
  if (list.state === "unavailable")
    return (
      <>
        <Alert
          tone="info"
          title="Token list: Not available on this server yet"
          className="spaced"
        >
          Expiry warnings and row-level revoke need the token list endpoint.
          Until then, revoke by token ID.
        </Alert>
        <RevokeById client={client} />
        <Needs block>token list (today: revoke by ID only)</Needs>
      </>
    );
  const items = list.data.items;
  const soon = items.filter((t) => {
    const at = t.expires_at ? Date.parse(t.expires_at) : NaN;
    return !t.revoked && at - Date.now() < 14 * DAY && at > Date.now();
  });
  return (
    <>
      {soon.length > 0 && (
        <Alert
          tone="warn"
          title={`${soon.length} token${soon.length === 1 ? "" : "s"} expire within 14 days`}
          className="spaced"
        >
          Issue a replacement before revoking.
        </Alert>
      )}
      <Table
        columns={[
          { key: "id", label: "Token", mono: true },
          {
            key: "principal",
            label: "Principal",
            mono: true,
            render: (t: TokenRecord) => name(t.principal_id),
          },
          {
            key: "scopes",
            label: "Scope",
            render: (t: TokenRecord) => (
              <Mono muted>
                {(t.scopes ?? [])
                  .map(
                    (s) =>
                      `${s.actions.join(",")} · ${s.namespace ?? s.feed_id ?? "*"}`,
                  )
                  .join("; ") || "—"}
              </Mono>
            ),
          },
          {
            key: "expires_at",
            label: "Expires",
            render: (t: TokenRecord) => when(t.expires_at),
          },
          {
            key: "last_used_at",
            label: "Last used",
            align: "right",
            render: (t: TokenRecord) =>
              t.last_used_at ? when(t.last_used_at) : "Never",
          },
          {
            key: "state",
            label: "Status",
            render: (t: TokenRecord) =>
              t.revoked ? (
                <Badge tone="neutral" dot>
                  Revoked
                </Badge>
              ) : t.expires_at && Date.parse(t.expires_at) < Date.now() ? (
                <Badge tone="bad" dot>
                  Expired
                </Badge>
              ) : soon.includes(t) ? (
                <Badge tone="warn" dot>
                  Expiring
                </Badge>
              ) : (
                <Badge tone="good" dot>
                  Active
                </Badge>
              ),
          },
          {
            key: "x",
            label: "",
            align: "right",
            render: (t: TokenRecord) =>
              t.revoked ? null : (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setRevoking(t)}
                >
                  Revoke
                </Button>
              ),
          },
        ]}
        rows={items}
      />
      {!items.length && <p className="muted">No tokens.</p>}
      {revoking && (
        <Modal
          title="Revoke token"
          description={`Token ${revoking.id} stops working immediately.`}
          onClose={() => setRevoking(undefined)}
          actions={
            <>
              <Button
                variant="secondary"
                onClick={() => setRevoking(undefined)}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={action.busy}
                onClick={async () => {
                  const ok = await action.run(() =>
                    client.call("revokeServiceToken", {
                      params: { id: revoking.id },
                    }),
                  );
                  if (ok) {
                    toast("Token revoked", revoking.id);
                    setRevoking(undefined);
                    list.reload();
                  }
                }}
              >
                {action.busy ? "Revoking…" : "Revoke token"}
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
