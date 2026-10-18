import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  FederatedPrincipal,
  FederationFeedBinding,
  FederationPolicyBody,
  FederationPolicyDiff,
  FederationPolicyHistory,
  FederationPolicyRole,
  FederationPolicyView,
  FederationStatus,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  CodeBlock,
  Input,
  Modal,
  PropertyList,
  Select,
  Table,
  Textarea,
} from "./ds/bundle";
import {
  Card,
  chk,
  Failure,
  Loading,
  Mono,
  SectionHead,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

const stalenessTone: Record<string, string> = {
  fresh: "good",
  warning: "warn",
  critical: "bad",
};

function age(seconds: number) {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

const emptyBody: FederationPolicyBody = {
  identity_domains: [],
  roles: [],
  principal_bindings: [],
  group_bindings: [],
  service_accounts: [],
  revocations: [],
};

function bodyOf(view?: FederationPolicyView): FederationPolicyBody {
  const d = view?.document;
  if (!d) return emptyBody;
  return {
    identity_domains: d.identity_domains,
    roles: d.roles,
    principal_bindings: d.principal_bindings,
    group_bindings: d.group_bindings,
    service_accounts: d.service_accounts,
    revocations: d.revocations,
  };
}

function diffSummary(d: FederationPolicyDiff) {
  const parts: string[] = [];
  for (const [label, s] of [
    ["identity domains", d.identity_domains],
    ["roles", d.roles],
    ["principal bindings", d.principal_bindings],
    ["group bindings", d.group_bindings],
    ["service accounts", d.service_accounts],
    ["revocations", d.revocations],
  ] as const) {
    const n = s.added.length + s.removed.length + s.changed.length;
    if (n)
      parts.push(
        `${label}: +${s.added.length} −${s.removed.length} ~${s.changed.length}`,
      );
  }
  return parts.length ? parts.join(" · ") : "no changes";
}

/** Federation policy, local bindings, settings and federated principals. */
export function FederationPolicy({
  client,
  st,
}: {
  client: Client;
  st: FederationStatus;
}) {
  const policy = useOptionalCall(client, "getFederationPolicy");
  const [editing, setEditing] = useState(false);
  const authority = st.role === "authority";
  const view = policy.state === "ready" ? policy.data : undefined;
  return (
    <>
      <SectionHead title="Policy">
        {authority && (
          <Button icon="edit" onClick={() => setEditing(true)}>
            Edit policy
          </Button>
        )}
      </SectionHead>
      {(st.alerts ?? []).map((a) => (
        <Alert
          key={a.code}
          tone={a.severity === "critical" ? "bad" : "warn"}
          title={a.code.replace(/_/g, " ")}
        >
          {a.message}
        </Alert>
      ))}
      {policy.state === "loading" && <Loading what="federation policy" />}
      {policy.state === "error" && <Failure error={policy.error} />}
      {view && <PolicySummary view={view} authority={authority} />}
      {view && (
        <FeedBindings client={client} federation={st.federation_name ?? ""} />
      )}
      {view && (
        <LocalSettings
          client={client}
          view={view}
          changed={() => policy.reload()}
        />
      )}
      {view && <Principals client={client} />}
      {editing && view && (
        <PolicyEditor
          client={client}
          view={view}
          onClose={() => setEditing(false)}
          done={() => {
            setEditing(false);
            policy.reload();
          }}
        />
      )}
    </>
  );
}

function PolicySummary({
  view,
  authority,
}: {
  view: FederationPolicyView;
  authority: boolean;
}) {
  const s = view.status;
  const doc = view.document;
  const [raw, setRaw] = useState(false);
  return (
    <>
      <Card
        title={
          <span className="row tight">
            {doc ? `Generation ${s.generation}` : "No policy published"}
            <Badge tone={stalenessTone[s.staleness]}>{s.staleness}</Badge>
          </span>
        }
        actions={
          doc && (
            <Button size="sm" variant="secondary" onClick={() => setRaw(!raw)}>
              {raw ? "Hide document" : "Show signed document"}
            </Button>
          )
        }
      >
        <PropertyList
          items={[
            {
              label: "Confirmed current",
              value: authority
                ? "Authority (source of policy)"
                : doc
                  ? `${age(s.age_seconds)} ago`
                  : "—",
            },
            { label: "Applied", value: when(s.applied_at) },
            {
              label: "Roles · principals",
              value: `${s.report.roles} federated roles · ${s.report.principals} federated principals · ${s.report.revoked} revoked`,
            },
            {
              label: "Unresolved feed keys",
              value: s.report.unresolved_feed_keys.length
                ? s.report.unresolved_feed_keys.join(", ") +
                  " (inert until bound)"
                : "none",
            },
            {
              label: "Federated instance admin",
              value: s.report.inert_admin_grants
                ? `${s.report.inert_admin_grants} grant(s) inert (not accepted locally)`
                : "none inert",
            },
            {
              label: "Last rejection",
              value: s.error ? `${s.error} (${when(s.error_at)})` : "none",
            },
          ]}
        />
        {raw && doc && (
          <CodeBlock title="policy" code={JSON.stringify(doc, null, 2)} />
        )}
      </Card>
      {doc && (
        <Table
          columns={[
            { key: "key", label: "Federated role", mono: true },
            { key: "revision", label: "Revision", align: "right" },
            {
              key: "grants",
              label: "Grants",
              render: (r: FederationPolicyRole) =>
                r.grants
                  .map(
                    (g) =>
                      `${g.effect} ${g.actions.join("+")} on ${g.kind}${g.feed_key ? " " + g.feed_key : ""}${g.namespace ? "/" + g.namespace : ""}`,
                  )
                  .join("; ") || "—",
            },
          ]}
          rows={doc.roles}
        />
      )}
      {view.history.length > 0 && (
        <Table
          columns={[
            { key: "generation", label: "Generation", align: "right" },
            {
              key: "issued_at",
              label: "Issued",
              render: (h: FederationPolicyHistory) => when(h.issued_at),
            },
            { key: "hash", label: "Hash", mono: true },
          ]}
          rows={view.history}
        />
      )}
    </>
  );
}

function PolicyEditor({
  client,
  view,
  onClose,
  done,
}: {
  client: Client;
  view: FederationPolicyView;
  onClose: () => void;
  done: () => void;
}) {
  const draft = useOptionalCall(client, "getFederationPolicyDraft");
  const loaded = draft.state === "ready" ? draft.data : undefined;
  const [text, setText] = useState<string>();
  const [diff, setDiff] = useState<FederationPolicyDiff>();
  const [etag, setEtag] = useState<string>();
  const [confirm, setConfirm] = useState(false);
  const action = useAction();
  const toast = useToast();
  const current =
    text ??
    JSON.stringify(loaded ? loaded.policy : bodyOf(view), null, 2) + "\n";
  let parsed: FederationPolicyBody | undefined;
  try {
    parsed = JSON.parse(current) as FederationPolicyBody;
  } catch {
    parsed = undefined;
  }
  const save = () =>
    action.run(async () => {
      const r = await client.call("saveFederationPolicyDraft", {
        body: { policy: parsed! },
        etag: etag ?? (loaded ? `"${loaded.revision}"` : undefined),
      });
      setDiff(r.data.diff);
      setEtag(r.etag ?? undefined);
      toast("Draft saved");
    });
  return (
    <Modal
      title="Edit federation policy"
      description="Draft the next policy generation. Publishing signs it with the authority key and pushes it to every member; members keep their local overrides."
      onClose={onClose}
      width={820}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="secondary"
            disabled={action.busy || !parsed}
            onClick={() => void save()}
          >
            Save draft
          </Button>
          <Button
            variant="danger"
            disabled={action.busy || !parsed || !confirm}
            onClick={async () => {
              if (
                await action.run(() =>
                  client.call("publishFederationPolicy", {
                    body: {
                      base_generation: view.status.generation,
                      confirm: true,
                      policy: parsed,
                    },
                  }),
                )
              ) {
                toast("Policy published");
                done();
              }
            }}
          >
            {action.busy ? "Working…" : "Publish"}
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      {draft.state === "loading" && <Loading what="draft" />}
      <Textarea
        label="Policy (JSON)"
        mono
        rows={18}
        value={current}
        onChange={val(setText)}
        error={parsed ? undefined : "Not valid JSON"}
        hint="identity_domains, roles (keys start with fed:), principal_bindings, group_bindings, service_accounts and revocations. Grants reference feed keys <federation>/<feed-slug>."
      />
      {(diff ?? loaded?.diff) && (
        <Alert tone="info" title="Changes against the applied generation">
          {diffSummary((diff ?? loaded?.diff)!)}
        </Alert>
      )}
      <Checkbox
        label={`Publish generation ${view.status.generation + 1} to every member`}
        description="Revocations and role changes take effect on all reachable members within seconds."
        checked={confirm}
        onChange={chk(setConfirm)}
      />
    </Modal>
  );
}

function FeedBindings({
  client,
  federation,
}: {
  client: Client;
  federation: string;
}) {
  const bindings = useOptionalCall(client, "listFederationFeedBindings");
  const feeds = useOptionalCall(client, "listFeeds");
  const [adding, setAdding] = useState(false);
  const [slug, setSlug] = useState("");
  const [feed, setFeed] = useState("");
  const action = useAction();
  const toast = useToast();
  const items = bindings.state === "ready" ? bindings.data.items : [];
  const feedItems = feeds.state === "ready" ? feeds.data.items : [];
  return (
    <>
      <SectionHead title="Feed keys" count={items.length}>
        <Button
          size="sm"
          variant="secondary"
          icon="link"
          onClick={() => setAdding(true)}
        >
          Bind feed
        </Button>
      </SectionHead>
      <Failure error={action.error} />
      {bindings.state === "ready" &&
        (items.length ? (
          <Table
            columns={[
              { key: "feed_key", label: "Feed key", mono: true },
              { key: "feed_slug", label: "Local feed" },
              {
                key: "actions",
                label: "",
                align: "right",
                render: (b: FederationFeedBinding) => (
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={action.busy}
                    onClick={async () => {
                      const [fed, s] = b.feed_key.split("/");
                      if (
                        await action.run(() =>
                          client.call("unbindFederationFeed", {
                            params: { fed, slug: s },
                          }),
                        )
                      ) {
                        toast("Feed key unbound");
                        bindings.reload();
                      }
                    }}
                  >
                    Unbind
                  </Button>
                ),
              },
            ]}
            rows={items}
          />
        ) : (
          <p className="muted">
            No feed keys bound. Policy grants on unbound keys are inert.
          </p>
        ))}
      {adding && (
        <Modal
          title="Bind a feed key"
          description="Grants in the federation policy that reference this key apply to the selected local feed."
          onClose={() => setAdding(false)}
          width={560}
          actions={
            <>
              <Button variant="secondary" onClick={() => setAdding(false)}>
                Cancel
              </Button>
              <Button
                disabled={
                  action.busy || !feed || !/^[a-z0-9][a-z0-9_-]*$/.test(slug)
                }
                onClick={async () => {
                  if (
                    await action.run(() =>
                      client.call("bindFederationFeed", {
                        body: {
                          feed_key: `${federation}/${slug}`,
                          feed_id: feed,
                        },
                      }),
                    )
                  ) {
                    toast("Feed key bound");
                    setAdding(false);
                    bindings.reload();
                  }
                }}
              >
                Bind
              </Button>
            </>
          }
        >
          <Failure error={action.error} />
          <Input
            label={`Feed key (${federation}/…)`}
            mono
            value={slug}
            onChange={val(setSlug)}
          />
          <Select
            label="Local feed"
            value={feed}
            onChange={val(setFeed)}
            options={[
              { value: "", label: "Choose a feed" },
              ...feedItems.map((f) => ({ value: f.id, label: f.slug })),
            ]}
          />
        </Modal>
      )}
    </>
  );
}

function LocalSettings({
  client,
  view,
  changed,
}: {
  client: Client;
  view: FederationPolicyView;
  changed: () => void;
}) {
  const [accept, setAccept] = useState(view.settings.accept_admin_grants);
  const [maxAge, setMaxAge] = useState(
    view.settings.policy_max_age_seconds
      ? String(view.settings.policy_max_age_seconds / 86400)
      : "",
  );
  const action = useAction();
  const toast = useToast();
  const days = maxAge.trim() === "" ? undefined : Number(maxAge);
  const valid = days === undefined || (days > 0 && days <= 365);
  return (
    <Card title="Local settings">
      <Failure error={action.error} />
      <Checkbox
        label="Accept federated instance-admin grants"
        description="Off by default: the federation cannot make anyone an administrator of this instance unless you accept it here."
        checked={accept}
        onChange={chk(setAccept)}
      />
      <Input
        label="Hard policy age limit (days)"
        value={maxAge}
        onChange={val(setMaxAge)}
        hint="Optional. When the policy has not been confirmed current for this long, federated principals are denied; local principals are unaffected. Empty keeps enforcing the last known good policy."
      />
      <Button
        disabled={action.busy || !valid}
        onClick={async () => {
          if (
            await action.run(() =>
              client.call("setFederationSettings", {
                body: {
                  accept_admin_grants: accept,
                  ...(days !== undefined
                    ? {
                        policy_max_age_seconds: Math.max(
                          300,
                          Math.round(days * 86400),
                        ),
                      }
                    : {}),
                },
              }),
            )
          ) {
            toast("Settings saved");
            changed();
          }
        }}
      >
        Save settings
      </Button>
    </Card>
  );
}

function Principals({ client }: { client: Client }) {
  const list = useOptionalCall(client, "listFederatedPrincipals");
  const action = useAction();
  const toast = useToast();
  if (list.state === "unavailable") return null;
  const items = list.state === "ready" ? list.data.items : [];
  return (
    <>
      <SectionHead title="Federated principals" count={items.length} />
      <Failure error={action.error} />
      {list.state === "error" && <Failure error={list.error} />}
      {list.state === "ready" &&
        (items.length ? (
          <Table
            columns={[
              {
                key: "subject",
                label: "Principal",
                render: (p: FederatedPrincipal) => (
                  <span className="row tight">
                    <Mono>{p.subject}</Mono>
                    <Badge>{p.kind}</Badge>
                    {p.disabled && <Badge tone="bad">revoked</Badge>}
                    {p.conflict && <Badge tone="warn">local conflict</Badge>}
                  </span>
                ),
              },
              {
                key: "roles",
                label: "Federated roles",
                render: (p: FederatedPrincipal) => p.roles.join(", ") || "—",
              },
              {
                key: "local_roles",
                label: "Local roles",
                render: (p: FederatedPrincipal) =>
                  p.local_roles.join(", ") || "—",
              },
              {
                key: "actions",
                label: "",
                align: "right",
                render: (p: FederatedPrincipal) =>
                  p.conflict ? (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={action.busy}
                      onClick={async () => {
                        if (
                          await action.run(() =>
                            client.call("adoptFederatedPrincipal", {
                              params: { id: p.id },
                              body: { confirm: true },
                            }),
                          )
                        ) {
                          toast("Principal adopted");
                          list.reload();
                        }
                      }}
                    >
                      Adopt
                    </Button>
                  ) : null,
              },
            ]}
            rows={items}
          />
        ) : (
          <p className="muted">No federated principals on this instance.</p>
        ))}
    </>
  );
}
