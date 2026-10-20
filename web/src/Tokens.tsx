import { useEffect, useState } from "react";
import type { Client } from "./api";
import { useAllFeeds } from "./data";
import { useOptionalCall } from "./gaps";
import { requireETag } from "./api";
import type {
  TokenCleanupResult,
  TokenCounts,
  TokenRecord,
  IdentityPrincipal,
  IssueServiceToken,
  IssuedServiceToken,
} from "./generated";
import { Actions } from "./Grants";
import {
  Alert,
  Button,
  IconButton,
  Input,
  Modal,
  SegmentedControl,
  Select,
  SettingsSection,
  Table,
} from "./ds/bundle";
import {
  DateCell,
  Dim,
  TOKEN_WARN_DAYS,
  TokenStateBadge,
  TokenVisibilityBar,
  expiresSoon,
  hiddenText,
  tokenState,
  useShowInactiveTokens,
} from "./TokenVisibility";
import {
  Failure,
  Loading,
  Mono,
  useAction,
  useResource,
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
  // Expired and revoked tokens are hidden unless the viewer opts in.
  const [show, setShow] = useShowInactiveTokens();
  const list = useOptionalCall(client, "listServiceTokens", {
    query: { limit: "200", state: show ? "all" : "active" },
  });
  const [counts, setCounts] = useState<TokenCounts>();
  useEffect(() => {
    if (list.state === "ready" && list.data.counts) setCounts(list.data.counts);
  }, [list]);
  const [revoking, setRevoking] = useState<TokenRecord>();
  const action = useAction();
  const toast = useToast();
  const name = (id?: string) =>
    principals.find((p) => p.id === id)?.subject ?? id ?? "—";
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
      </>
    );
  const bar = (
    <TokenVisibilityBar show={show} onShow={setShow} counts={counts} />
  );
  if (list.state === "loading")
    return (
      <>
        {bar}
        <Loading what="tokens" />
      </>
    );
  if (list.state === "error")
    return (
      <>
        {bar}
        <Failure error={list.error} />
      </>
    );
  const items = list.data.items;
  const soon = items.filter((t) => expiresSoon(t) !== null);
  const hidden = hiddenText(show ? undefined : (list.data.counts ?? counts));
  return (
    <>
      {bar}
      {soon.length > 0 && (
        <Alert
          tone="warn"
          title={`${soon.length} token${soon.length === 1 ? "" : "s"} expire within ${TOKEN_WARN_DAYS} days`}
          className="spaced"
        >
          Issue a replacement before revoking.
        </Alert>
      )}
      <Table
        columns={[
          {
            key: "id",
            label: "Token",
            mono: true,
            render: (t: TokenRecord) => (
              <Dim token={t}>
                <span title={t.id}>{t.id.slice(0, 8)}</span>
              </Dim>
            ),
          },
          {
            key: "principal",
            label: "Principal",
            mono: true,
            render: (t: TokenRecord) => (
              <Dim token={t}>{name(t.principal_id)}</Dim>
            ),
          },
          {
            key: "scopes",
            label: "Scope",
            render: (t: TokenRecord) => (
              <Dim token={t}>
                <Mono muted className="wrap-cell">
                  {(t.scopes ?? [])
                    .map(
                      (s) =>
                        `${s.actions.join(", ")} · ${s.namespace ?? s.feed_id ?? "*"}`,
                    )
                    .join("; ") || "—"}
                </Mono>
              </Dim>
            ),
          },
          {
            key: "expires_at",
            label: "Expires",
            render: (t: TokenRecord) => (
              <Dim token={t}>
                <DateCell value={t.expires_at} />
              </Dim>
            ),
          },
          {
            key: "last_used_at",
            label: "Last used",
            align: "right",
            render: (t: TokenRecord) => (
              <Dim token={t}>
                {t.last_used_at ? <DateCell value={t.last_used_at} /> : "Never"}
              </Dim>
            ),
          },
          {
            key: "state",
            label: "Status",
            render: (t: TokenRecord) => <TokenStateBadge token={t} />,
          },
          {
            key: "x",
            label: "",
            align: "right",
            render: (t: TokenRecord) =>
              tokenState(t) !== "active" ? null : (
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
      {!items.length && (
        <p className="muted">
          {show ? "No tokens." : "No active tokens."}
          {hidden ? ` ${hidden}.` : ""}
        </p>
      )}
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

const CLEANUP_KEY = "token_cleanup_after_days";

// Admin-only: how long expired and revoked tokens are kept, and an immediate
// cleanup that previews the count first.
function TokenCleanup({
  client,
  onCleaned,
}: {
  client: Client;
  onCleaned: () => void;
}) {
  const settings = useOptionalCall(client, "getSettings");
  const [days, setDays] = useState<string>();
  const [preview, setPreview] = useState<TokenCleanupResult>();
  const save = useAction();
  const run = useAction();
  const toast = useToast();
  if (settings.state !== "ready") return null;
  const item = settings.data.items.find((i) => i.key === CLEANUP_KEY);
  // Servers without the setting simply do not offer cleanup.
  if (!item) return null;
  const current = item.value;
  const value = days ?? current;
  const valid = /^\d+$/.test(value) && Number(value) <= 3650;
  const dirty = value !== current;
  const never = Number(current) === 0;
  return (
    <div className="stack tok-cleanup" data-testid="token-cleanup">
      <h3 className="h3">Cleanup</h3>
      <form
        className="row end"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await save.run(() =>
            client.call("putSettings", {
              etag: requireETag(`"${settings.data.revision}"`),
              body: { token_cleanup_after_days: Number(value) },
            }),
          );
          if (ok) {
            toast("Token cleanup saved");
            setDays(undefined);
            settings.reload();
          }
        }}
      >
        <Input
          label="Delete expired or revoked tokens after (days)"
          type="number"
          min={0}
          max={3650}
          value={value}
          disabled={item.read_only}
          onChange={val(setDays)}
        />
        <Button
          type="submit"
          disabled={item.read_only || !dirty || !valid || save.busy}
        >
          {save.busy ? "Saving…" : "Save"}
        </Button>
        <Button
          type="button"
          variant="secondary"
          icon="trash-2"
          disabled={never || dirty || run.busy}
          onClick={async () => {
            let result: TokenCleanupResult | undefined;
            const ok = await run.run(async () => {
              result = (
                await client.call("cleanupTokens", { body: { dry_run: true } })
              ).data;
            });
            if (ok) setPreview(result);
          }}
        >
          Clean up now
        </Button>
      </form>
      <p className="muted small">
        {item.read_only
          ? "Set by the deployment (ANT_TOKEN_CLEANUP_AFTER_DAYS) and read-only here. "
          : ""}
        {never
          ? "Expired and revoked tokens are kept forever. "
          : `Tokens that have been expired or revoked for more than ${current} days are removed automatically. `}
        0 keeps them forever; the default is 30. Audit history keeps the token
        ID.
      </p>
      <Failure error={save.error ?? run.error} />
      {preview && (
        <Modal
          title="Clean up tokens"
          description={
            preview.total > 0
              ? `Delete ${preview.total} token${preview.total === 1 ? "" : "s"} expired or revoked more than ${preview.after_days} days ago?`
              : `No tokens have been expired or revoked for more than ${preview.after_days} days.`
          }
          onClose={() => setPreview(undefined)}
          actions={
            preview.total > 0 ? (
              <>
                <Button
                  variant="secondary"
                  onClick={() => setPreview(undefined)}
                >
                  Cancel
                </Button>
                <Button
                  variant="danger"
                  disabled={run.busy}
                  onClick={async () => {
                    let done: TokenCleanupResult | undefined;
                    const ok = await run.run(async () => {
                      done = (
                        await client.call("cleanupTokens", {
                          body: { dry_run: false },
                        })
                      ).data;
                    });
                    if (ok && done) {
                      toast(
                        `Deleted ${done.total} token${done.total === 1 ? "" : "s"}`,
                        done.has_more
                          ? "More remain; run it again."
                          : undefined,
                      );
                      setPreview(undefined);
                      onCleaned();
                    }
                  }}
                >
                  {run.busy ? "Deleting…" : `Delete ${preview.total}`}
                </Button>
              </>
            ) : (
              <Button onClick={() => setPreview(undefined)}>Close</Button>
            )
          }
        >
          <p className="muted small">
            {preview.service_tokens} service account and{" "}
            {preview.personal_access_tokens} personal access tokens. Active
            tokens are never deleted. This is recorded in the audit log.
          </p>
          <Failure error={run.error} />
        </Modal>
      )}
    </div>
  );
}

// Admin-only: service-account tokens (non-human principals), separate from
// the signed-in user's own personal access tokens.
export function ServiceTokensSection({ client }: { client: Client }) {
  const principals = useResource(
    () => client.call("listPrincipals", { query: { limit: "200" } }),
    [client],
  );
  const [issuing, setIssuing] = useState(false);
  const [version, setVersion] = useState(0);
  const items = principals.value?.data.items ?? [];
  return (
    <SettingsSection
      title="Service account tokens"
      description="Tokens issued to service principals for automation (CI, replication, mirrors). They are not tied to your account. Administrators only."
    >
      <div>
        <Button
          icon="key-round"
          disabled={principals.loading || !!principals.error}
          onClick={() => setIssuing(true)}
        >
          Issue service token
        </Button>
      </div>
      {principals.error ? (
        <Failure error={principals.error} />
      ) : principals.loading ? (
        <Loading what="service principals" />
      ) : (
        <>
          <TokensTab key={version} client={client} principals={items} />
          <TokenCleanup
            client={client}
            onCleaned={() => setVersion((v) => v + 1)}
          />
        </>
      )}
      {issuing && (
        <TokenIssuer
          client={client}
          principals={items}
          onClose={() => {
            setIssuing(false);
            setVersion((v) => v + 1);
          }}
        />
      )}
    </SettingsSection>
  );
}
