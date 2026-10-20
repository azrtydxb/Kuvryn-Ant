import { useEffect, useState } from "react";
import { APIError, type Client } from "./api";
import { useAllFeeds } from "./data";
import { isUnavailable } from "./gaps";
import type {
  ClientAuthMethod,
  ClientAuthPolicy,
  CreatedPersonalAccessToken,
  GitHubTrustRule,
  GitHubTrustRuleInput,
  IdentityPrincipal,
  NetworkGroup,
  PersonalAccessToken,
  TokenCounts,
  TokenScope,
} from "./generated";
import {
  DateCell,
  Dim,
  TokenStateBadge,
  TokenVisibilityBar,
  hiddenText,
  tokenState,
  useShowInactiveTokens,
} from "./TokenVisibility";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  IconButton,
  Input,
  Modal,
  SegmentedControl,
  Select,
  SettingsRow,
  SettingsSection,
  Table,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Unavailable,
  chk,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

export const methodInfo: Record<
  ClientAuthMethod,
  { label: string; text: string }
> = {
  anonymous: {
    label: "Anonymous",
    text: "Read-only, no credentials. Only on feeds that allow anonymous reads. Publishing is never anonymous.",
  },
  token: { label: "Service token", text: "ant_… machine tokens." },
  "basic-directory": {
    label: "Directory password",
    text: "HTTP Basic user and password checked against the configured LDAP or Active Directory.",
  },
  pat: {
    label: "Personal access token",
    text: "antp_… tokens users create under Settings → Profile. Works as a Bearer or as a Basic password.",
  },
  "github-oidc": {
    label: "GitHub Actions OIDC",
    text: "Workflow identity tokens matched by trust rules, no stored secret.",
  },
};
const methodOrder = Object.keys(methodInfo) as ClientAuthMethod[];
const actionChoices = ["read", "publish", "delete"] as const;

/* ---------- policy editor shared by the instance default and each feed ---------- */

function PolicyForm({
  policy,
  groups,
  canEditGroups,
  title,
  description,
  onSave,
  onReset,
}: {
  policy: ClientAuthPolicy;
  groups: NetworkGroup[];
  canEditGroups: boolean;
  title: string;
  description: string;
  onSave: (methods: ClientAuthMethod[], groupIDs: string[]) => Promise<unknown>;
  onReset?: () => Promise<unknown>;
}) {
  const [methods, setMethods] = useState<ClientAuthMethod[]>(policy.methods);
  const [selected, setSelected] = useState<string[]>(
    policy.anonymous_network_group_ids,
  );
  const action = useAction();
  const toast = useToast();
  const anonymous = methods.includes("anonymous");
  const toggle = (m: ClientAuthMethod, on: boolean) =>
    setMethods(methodOrder.filter((k) => (k === m ? on : methods.includes(k))));
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        const ok = await action.run(() =>
          onSave(methods, anonymous ? selected : []),
        );
        if (ok) toast("Access policy saved");
      }}
    >
      <SettingsSection
        title={title}
        description={description}
        form
        footer={
          <span className="row">
            <Button type="submit" disabled={action.busy || !methods.length}>
              {action.busy ? "Saving…" : "Save access"}
            </Button>
            {onReset && !policy.inherited && (
              <Button
                type="button"
                variant="secondary"
                disabled={action.busy}
                onClick={async () => {
                  if (await action.run(onReset))
                    toast("Using the instance default");
                }}
              >
                Use instance default
              </Button>
            )}
          </span>
        }
        footerNote={
          policy.inherited
            ? "Inherited from the instance default."
            : `Revision ${policy.revision}`
        }
      >
        <fieldset className="plain">
          <legend>Accepted client authentication</legend>
          <div className="stack">
            {methodOrder.map((m) => (
              <Checkbox
                key={m}
                label={methodInfo[m].label}
                description={methodInfo[m].text}
                checked={methods.includes(m)}
                onChange={chk((on) => toggle(m, on))}
              />
            ))}
          </div>
        </fieldset>
        {anonymous && (
          <fieldset className="plain">
            <legend>Anonymous networks</legend>
            {groups.length ? (
              <div className="row wrap">
                {groups.map((g) => (
                  <Checkbox
                    key={g.id}
                    label={g.name}
                    description={g.cidrs.join(", ")}
                    checked={selected.includes(g.id)}
                    onChange={chk((on) =>
                      setSelected(
                        on
                          ? [...selected, g.id]
                          : selected.filter((x) => x !== g.id),
                      ),
                    )}
                  />
                ))}
              </div>
            ) : (
              <p className="muted small">
                {canEditGroups
                  ? "No network groups yet. Define them under Settings → Access."
                  : "Network groups are managed by an instance administrator."}
              </p>
            )}
            <p className="muted small">
              {selected.length
                ? "Anonymous reads are accepted only from the selected networks."
                : "No network selected: anonymous reads are accepted from any network."}
            </p>
          </fieldset>
        )}
        {!methods.length && (
          <Alert tone="warn" title="No method selected">
            Select at least one method.
          </Alert>
        )}
        <Failure error={action.error} />
      </SettingsSection>
    </form>
  );
}

/* ---------- Feeds → Settings → Access ---------- */

export function FeedAccess({
  client,
  feedId,
}: {
  client: Client;
  feedId: string;
}) {
  const r = useResource(async () => {
    const policy = await client.call("getFeedClientAuthPolicy", {
      params: { id: feedId },
    });
    // Groups need instance admin; without it the editor still works for methods.
    const groups = await client
      .call("listNetworkGroups")
      .then((g) => ({ items: g.data.items, admin: true }))
      .catch(() => ({ items: [] as NetworkGroup[], admin: false }));
    return { data: { policy: policy.data, ...groups }, etag: null };
  }, [client, feedId]);
  if (r.loading) return <Loading what="access policy" />;
  if (r.error)
    return isUnavailable(r.error) ? (
      <Unavailable what="Client access policy" />
    ) : (
      <Failure error={r.error} />
    );
  const { policy, items, admin } = r.value!.data;
  return (
    <PolicyForm
      key={policy.revision + ":" + policy.inherited}
      policy={policy}
      groups={items}
      canEditGroups={admin}
      title="Access"
      description="Which client credentials this feed accepts for package traffic. This is separate from upstream credentials on remote sources."
      onSave={async (methods, ids) => {
        await client.call("putFeedClientAuthPolicy", {
          params: { id: feedId },
          body: { methods, anonymous_network_group_ids: ids },
        });
        r.reload();
      }}
      onReset={async () => {
        await client.call("deleteFeedClientAuthPolicy", {
          params: { id: feedId },
        });
        r.reload();
      }}
    />
  );
}

/* ---------- Settings → Access (instance) ---------- */

export function AccessPane({ client }: { client: Client }) {
  const policy = useResource(
    () => client.call("getClientAuthPolicy"),
    [client],
  );
  const groups = useResource(() => client.call("listNetworkGroups"), [client]);
  if (policy.loading || groups.loading) return <Loading what="access" />;
  const failed = policy.error ?? groups.error;
  if (failed)
    return isUnavailable(failed) ? (
      <Unavailable what="Client access policy" />
    ) : (
      <Failure error={failed} />
    );
  return (
    <>
      <PolicyForm
        key={policy.value!.data.revision}
        policy={policy.value!.data}
        groups={groups.value!.data.items}
        canEditGroups
        title="Default client authentication"
        description="Applies to every feed without its own policy (Feeds → Settings → Access)."
        onSave={async (methods, ids) => {
          await client.call("putClientAuthPolicy", {
            body: { methods, anonymous_network_group_ids: ids },
          });
          policy.reload();
        }}
      />
      <NetworkGroups
        client={client}
        groups={groups.value!.data.items}
        reload={() => {
          groups.reload();
          policy.reload();
        }}
      />
      <GitHubOIDC client={client} />
    </>
  );
}

/* ---------- network groups ---------- */

function NetworkGroups({
  client,
  groups,
  reload,
}: {
  client: Client;
  groups: NetworkGroup[];
  reload: () => void;
}) {
  const [editing, setEditing] = useState<NetworkGroup | "new">();
  const action = useAction();
  const toast = useToast();
  return (
    <SettingsSection
      title="Network groups"
      description="Named CIDR lists such as cluster or lan, used to restrict anonymous reads."
    >
      <Table
        columns={[
          { key: "name", label: "Name", mono: true },
          {
            key: "cidrs",
            label: "Networks",
            render: (g: NetworkGroup) => (
              <Mono muted>{g.cidrs.join(", ")}</Mono>
            ),
          },
          {
            key: "x",
            label: "",
            align: "right",
            render: (g: NetworkGroup) => (
              <span className="row tight end">
                <Button size="sm" variant="ghost" onClick={() => setEditing(g)}>
                  Edit
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={action.busy}
                  onClick={async () => {
                    if (
                      await action.run(() =>
                        client.call("deleteNetworkGroup", {
                          params: { id: g.id },
                        }),
                      )
                    ) {
                      toast("Network group deleted", g.name);
                      reload();
                    }
                  }}
                >
                  Delete
                </Button>
              </span>
            ),
          },
        ]}
        rows={groups}
      />
      {!groups.length && <p className="muted">No network groups.</p>}
      <Failure error={action.error} />
      <div>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() => setEditing("new")}
        >
          Add network group
        </Button>
      </div>
      {editing && (
        <GroupEditor
          client={client}
          group={editing === "new" ? undefined : editing}
          onClose={() => setEditing(undefined)}
          saved={() => {
            setEditing(undefined);
            reload();
          }}
        />
      )}
    </SettingsSection>
  );
}

function GroupEditor({
  client,
  group,
  onClose,
  saved,
}: {
  client: Client;
  group?: NetworkGroup;
  onClose: () => void;
  saved: () => void;
}) {
  const [name, setName] = useState(group?.name ?? "");
  const [cidrs, setCidrs] = useState((group?.cidrs ?? []).join("\n"));
  const action = useAction();
  const list = cidrs
    .split(/[\s,]+/)
    .map((c) => c.trim())
    .filter(Boolean);
  const valid = /^[a-z0-9][a-z0-9_-]{0,62}$/.test(name) && list.length > 0;
  return (
    <Modal
      title={group ? "Edit network group" : "Add network group"}
      description="One CIDR per line, for example 10.0.0.0/8."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!valid || action.busy}
            onClick={async () => {
              const body = { name, cidrs: list };
              const ok = await action.run(() =>
                group
                  ? client.call("putNetworkGroup", {
                      params: { id: group.id },
                      body,
                    })
                  : client.call("createNetworkGroup", { body }),
              );
              if (ok) saved();
            }}
          >
            {action.busy ? "Saving…" : "Save"}
          </Button>
        </>
      }
    >
      <Input
        label="Name"
        mono
        value={name}
        onChange={val(setName)}
        hint="Lowercase letters, digits, _ or -."
      />
      <label className="az-field">
        <span className="az-field__label">Networks (CIDR)</span>
        <textarea
          className="az-input mono"
          rows={5}
          value={cidrs}
          onChange={val(setCidrs)}
        />
      </label>
      <Failure error={action.error} />
    </Modal>
  );
}

/* ---------- GitHub Actions OIDC ---------- */

function GitHubOIDC({ client }: { client: Client }) {
  const config = useResource(
    () => client.call("getGitHubOIDCConfig"),
    [client],
  );
  const rules = useResource(
    () => client.call("listGitHubTrustRules"),
    [client],
  );
  const [audience, setAudience] = useState<string>();
  const [editing, setEditing] = useState<GitHubTrustRule | "new">();
  const action = useAction();
  const toast = useToast();
  if (config.loading || rules.loading) return <Loading what="GitHub OIDC" />;
  if (config.error || rules.error)
    return <Failure error={config.error ?? rules.error} />;
  const current = config.value!.data;
  const aud = audience ?? current.audience;
  const items = rules.value!.data.items;
  return (
    <>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("putGitHubOIDCConfig", { body: { audience: aud } }),
          );
          if (ok) {
            toast("GitHub OIDC settings saved");
            setAudience(undefined);
            config.reload();
          }
        }}
      >
        <SettingsSection
          title="GitHub Actions OIDC"
          description="Workflows exchange their OIDC token for a short-lived Ant token. Leave the audience empty to disable."
          form
          footer={
            <Button type="submit" disabled={action.busy}>
              {action.busy ? "Saving…" : "Save"}
            </Button>
          }
        >
          <Input
            label="Audience"
            mono
            value={aud}
            maxLength={256}
            onChange={val(setAudience)}
            hint="Workflows request tokens with this audience, for example your public URL."
          />
          <SettingsRow label="Issuer" description="Fixed for GitHub Actions.">
            <Mono muted>{current.issuer}</Mono>
          </SettingsRow>
          <Failure error={action.error} />
        </SettingsSection>
      </form>
      <SettingsSection
        title="Trust rules"
        description="The oldest enabled rule that matches a workflow decides its access. Access is the service principal's grants narrowed by the rule's scopes."
      >
        <Table
          columns={[
            { key: "name", label: "Rule", mono: true },
            {
              key: "match",
              label: "Matches",
              render: (r: GitHubTrustRule) => (
                <Mono muted>
                  {r.org}/{r.repo} · ref {r.ref} · env {r.environment} · wf{" "}
                  {r.workflow}
                </Mono>
              ),
            },
            {
              key: "scopes",
              label: "Grants",
              render: (r: GitHubTrustRule) => (
                <Mono muted>{scopeText(r.scopes)}</Mono>
              ),
            },
            {
              key: "ttl",
              label: "Lifetime",
              render: (r: GitHubTrustRule) => `${r.ttl_seconds / 60} min`,
            },
            {
              key: "state",
              label: "State",
              render: (r: GitHubTrustRule) => (
                <Badge tone={r.enabled ? "good" : "neutral"} dot>
                  {r.enabled ? "Enabled" : "Disabled"}
                </Badge>
              ),
            },
            {
              key: "x",
              label: "",
              align: "right",
              render: (r: GitHubTrustRule) => (
                <span className="row tight end">
                  <Button
                    size="sm"
                    variant="ghost"
                    onClick={() => setEditing(r)}
                  >
                    Edit
                  </Button>
                  <Button
                    size="sm"
                    variant="ghost"
                    disabled={action.busy}
                    onClick={async () => {
                      if (
                        await action.run(() =>
                          client.call("deleteGitHubTrustRule", {
                            params: { id: r.id },
                          }),
                        )
                      ) {
                        toast("Trust rule deleted", r.name);
                        rules.reload();
                      }
                    }}
                  >
                    Delete
                  </Button>
                </span>
              ),
            },
          ]}
          rows={items}
        />
        {!items.length && <p className="muted">No trust rules.</p>}
        <div>
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            onClick={() => setEditing("new")}
          >
            Add trust rule
          </Button>
        </div>
        {editing && (
          <RuleEditor
            client={client}
            rule={editing === "new" ? undefined : editing}
            onClose={() => setEditing(undefined)}
            saved={() => {
              setEditing(undefined);
              rules.reload();
            }}
          />
        )}
      </SettingsSection>
    </>
  );
}

function scopeText(scopes: TokenScope[]) {
  return (
    scopes
      .map((s) => `${s.actions.join(",")} · ${s.namespace ?? s.feed_id ?? "*"}`)
      .join("; ") || "—"
  );
}

function RuleEditor({
  client,
  rule,
  onClose,
  saved,
}: {
  client: Client;
  rule?: GitHubTrustRule;
  onClose: () => void;
  saved: () => void;
}) {
  const feeds = useAllFeeds(client);
  const principals = useResource(
    () => client.call("listPrincipals", { query: { limit: "200" } }),
    [client],
  );
  const services = (principals.value?.data.items ?? []).filter(
    (p: IdentityPrincipal) => p.kind === "service" && !p.disabled,
  );
  const [f, setF] = useState({
    name: rule?.name ?? "",
    org: rule?.org ?? "",
    repo: rule?.repo ?? "*",
    ref: rule?.ref ?? "main",
    environment: rule?.environment ?? "*",
    workflow: rule?.workflow ?? "*",
    principal: rule?.principal_id ?? "",
    feed: rule?.scopes[0]?.feed_id ?? "",
    actions: rule?.scopes[0]?.actions ?? (["read"] as TokenScope["actions"]),
    ttl: String((rule?.ttl_seconds ?? 900) / 60),
    enabled: rule?.enabled ?? true,
  });
  const set = (k: keyof typeof f) => val((v) => setF({ ...f, [k]: v }));
  const action = useAction();
  const principal = f.principal || services[0]?.id || "";
  const ttl = Math.round(Number(f.ttl) * 60);
  const valid =
    f.name &&
    /^[A-Za-z0-9][A-Za-z0-9-]{0,38}$/.test(f.org) &&
    f.repo &&
    f.ref &&
    f.environment &&
    f.workflow &&
    principal &&
    f.actions.length > 0 &&
    ttl >= 60 &&
    ttl <= 3600;
  return (
    <Modal
      title={rule ? "Edit trust rule" : "Add trust rule"}
      description="Patterns accept * and ?. Branch names such as main match refs/heads/main; use refs/tags/v* for tags."
      onClose={onClose}
      width={620}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!valid || action.busy}
            onClick={async () => {
              const body: GitHubTrustRuleInput = {
                name: f.name,
                org: f.org,
                repo: f.repo,
                ref: f.ref,
                environment: f.environment,
                workflow: f.workflow,
                principal_id: principal,
                scopes: [
                  {
                    actions: f.actions,
                    ...(f.feed ? { feed_id: f.feed } : {}),
                  },
                ],
                ttl_seconds: ttl,
                enabled: f.enabled,
              };
              const ok = await action.run(() =>
                rule
                  ? client.call("putGitHubTrustRule", {
                      params: { id: rule.id },
                      body,
                    })
                  : client.call("createGitHubTrustRule", { body }),
              );
              if (ok) saved();
            }}
          >
            {action.busy ? "Saving…" : "Save rule"}
          </Button>
        </>
      }
    >
      <Input label="Rule name" mono value={f.name} onChange={set("name")} />
      <div className="cols">
        <Input
          label="Organization"
          mono
          value={f.org}
          onChange={set("org")}
          hint="Exact owner, case-insensitive."
        />
        <Input
          label="Repository"
          mono
          value={f.repo}
          onChange={set("repo")}
          hint="Name without owner, * for all."
        />
      </div>
      <div className="cols">
        <Input label="Ref / branch" mono value={f.ref} onChange={set("ref")} />
        <Input
          label="Environment"
          mono
          value={f.environment}
          onChange={set("environment")}
        />
        <Input
          label="Workflow"
          mono
          value={f.workflow}
          onChange={set("workflow")}
        />
      </div>
      <Select
        label="Acts as (service principal)"
        options={
          services.length
            ? services.map((p) => ({ value: p.id, label: p.subject }))
            : [{ value: "", label: "No enabled service principals" }]
        }
        value={principal}
        onChange={set("principal")}
      />
      <Select
        label="Feed"
        options={[
          { value: "", label: "All feeds" },
          ...feeds.items.map((x) => ({ value: x.id, label: x.slug })),
        ]}
        value={f.feed}
        onChange={set("feed")}
      />
      <ActionPicker
        value={f.actions}
        onChange={(actions) => setF({ ...f, actions })}
      />
      <div className="cols">
        <Input
          label="Token lifetime (minutes)"
          type="number"
          mono
          value={f.ttl}
          onChange={set("ttl")}
          error={
            ttl >= 60 && ttl <= 3600 ? undefined : "Between 1 and 60 minutes."
          }
        />
        <Checkbox
          label="Enabled"
          checked={f.enabled}
          onChange={chk((enabled) => setF({ ...f, enabled }))}
        />
      </div>
      <Failure error={action.error} />
    </Modal>
  );
}

function ActionPicker({
  value,
  onChange,
}: {
  value: TokenScope["actions"];
  onChange: (v: TokenScope["actions"]) => void;
}) {
  return (
    <fieldset className="plain actions">
      <legend>Operations (select at least one)</legend>
      <div className="row wrap">
        {actionChoices.map((a) => (
          <Checkbox
            key={a}
            label={a}
            checked={value.includes(a)}
            onChange={chk((on) =>
              onChange(
                actionChoices.filter((x) => (x === a ? on : value.includes(x))),
              ),
            )}
          />
        ))}
      </div>
    </fieldset>
  );
}

/* ---------- Settings → Profile → Access tokens ---------- */

const durations: Record<string, number> = {
  "30 d": 30,
  "90 d": 90,
  "180 d": 180,
  "1 year": 365,
};

export function PersonalTokensPane({ client }: { client: Client }) {
  // Expired and revoked tokens are hidden unless the viewer opts in.
  const [show, setShow] = useShowInactiveTokens();
  const list = useResource(
    () =>
      client.call("listPersonalAccessTokens", {
        query: { state: show ? "all" : "active" },
      }),
    [client, show],
  );
  const [counts, setCounts] = useState<TokenCounts>();
  useEffect(() => {
    const c = list.value?.data.counts;
    if (c) setCounts(c);
  }, [list.value]);
  const [creating, setCreating] = useState(false);
  const [revoking, setRevoking] = useState<PersonalAccessToken>();
  const [cleaning, setCleaning] = useState(false);
  const action = useAction();
  const toast = useToast();
  if (list.error)
    return isUnavailable(list.error) ? (
      <Unavailable what="Personal access tokens" />
    ) : list.error instanceof APIError && list.error.status === 403 ? (
      <SettingsSection title="Personal access tokens">
        <Alert tone="info" title="Sign in as a person to manage these">
          Personal access tokens belong to a signed-in user. This session uses a
          service token, which cannot own them.
        </Alert>
      </SettingsSection>
    ) : (
      <Failure error={list.error} />
    );
  const items = list.value?.data.items;
  const inactive = (counts?.expired ?? 0) + (counts?.revoked ?? 0);
  return (
    <SettingsSection
      title="Personal access tokens"
      description="Tokens act as you, limited to the scopes you choose. Use one as a Bearer token or as the Basic password with any username. Secrets are shown only once."
    >
      <div>
        <Button icon="key-round" onClick={() => setCreating(true)}>
          Create token
        </Button>
      </div>
      <TokenVisibilityBar show={show} onShow={setShow} counts={counts}>
        {inactive > 0 && (
          <Button
            size="sm"
            variant="ghost"
            icon="trash-2"
            onClick={() => setCleaning(true)}
          >
            Delete expired
          </Button>
        )}
      </TokenVisibilityBar>
      {!items ? (
        <Loading what="personal access tokens" />
      ) : (
        <Table
          columns={[
            {
              key: "name",
              label: "Name",
              render: (t: PersonalAccessToken) => <Dim token={t}>{t.name}</Dim>,
            },
            {
              key: "scopes",
              label: "Scope",
              render: (t: PersonalAccessToken) => (
                <Dim token={t}>
                  <Mono muted>{scopeText(t.scopes)}</Mono>
                </Dim>
              ),
            },
            {
              key: "expires_at",
              label: "Expires",
              render: (t: PersonalAccessToken) => (
                <Dim token={t}>
                  <DateCell value={t.expires_at} />
                </Dim>
              ),
            },
            {
              key: "last_used_at",
              label: "Last used",
              align: "right",
              render: (t: PersonalAccessToken) => (
                <Dim token={t}>
                  {t.last_used_at ? (
                    <DateCell value={t.last_used_at} />
                  ) : (
                    "Never"
                  )}
                </Dim>
              ),
            },
            {
              key: "state",
              label: "Status",
              render: (t: PersonalAccessToken) => <TokenStateBadge token={t} />,
            },
            {
              key: "x",
              label: "",
              align: "right",
              render: (t: PersonalAccessToken) =>
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
      )}
      {items && !items.length && (
        <p className="muted">
          {show ? "No personal access tokens." : "No active tokens."}
          {!show && inactive > 0 ? ` ${hiddenText(counts)}.` : ""}
        </p>
      )}
      {cleaning && (
        <Modal
          title="Delete expired tokens"
          description={`Delete your ${inactive} expired or revoked personal access token${inactive === 1 ? "" : "s"}? Active tokens and other people's tokens are not touched.`}
          onClose={() => setCleaning(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setCleaning(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={action.busy}
                onClick={async () => {
                  let deleted = 0;
                  const ok = await action.run(async () => {
                    deleted = (await client.call("cleanupPersonalAccessTokens"))
                      .data.deleted;
                  });
                  if (ok) {
                    toast(
                      `Deleted ${deleted} token${deleted === 1 ? "" : "s"}`,
                    );
                    setCleaning(false);
                    list.reload();
                  }
                }}
              >
                {action.busy ? "Deleting…" : "Delete expired tokens"}
              </Button>
            </>
          }
        >
          <Failure error={action.error} />
        </Modal>
      )}
      {creating && (
        <PersonalTokenCreator
          client={client}
          onClose={() => {
            setCreating(false);
            list.reload();
          }}
        />
      )}
      {revoking && (
        <Modal
          title="Revoke token"
          description={`${revoking.name} stops working immediately.`}
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
                    client.call("revokePersonalAccessToken", {
                      params: { id: revoking.id },
                    }),
                  );
                  if (ok) {
                    toast("Token revoked", revoking.name);
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
    </SettingsSection>
  );
}

function PersonalTokenCreator({
  client,
  onClose,
}: {
  client: Client;
  onClose: () => void;
}) {
  const feeds = useAllFeeds(client);
  const [name, setName] = useState("");
  const [feed, setFeed] = useState("");
  const [namespace, setNamespace] = useState("");
  const [actions, setActions] = useState<TokenScope["actions"]>(["read"]);
  const [duration, setDuration] = useState("90 d");
  const [created, setCreated] = useState<CreatedPersonalAccessToken>();
  const [attempted, setAttempted] = useState(false);
  const [copied, setCopied] = useState(false);
  const action = useAction();
  const toast = useToast();
  // The secret is shown once and cleared after a minute or when leaving.
  useEffect(() => {
    if (!created) return;
    const timer = setTimeout(() => setCreated(undefined), 60000);
    return () => clearTimeout(timer);
  }, [created]);
  return (
    <Modal
      title={created ? "Token created" : "Create personal access token"}
      description={
        created
          ? "Copy it now. It is shown once and cleared in 60 s."
          : "The token can do what you can, limited to the scope below. Creation is never retried automatically."
      }
      onClose={onClose}
      actions={
        created ? (
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
              Create a separate token
            </Button>
          </>
        ) : (
          <>
            <Button variant="secondary" onClick={onClose}>
              Cancel
            </Button>
            <Button
              icon="key-round"
              disabled={action.busy || !name.trim() || !actions.length}
              onClick={async () => {
                const ok = await action.run(async () => {
                  setAttempted(true);
                  const r = await client.call("createPersonalAccessToken", {
                    body: {
                      name: name.trim(),
                      scopes: [
                        {
                          actions,
                          ...(feed ? { feed_id: feed } : {}),
                          ...(namespace.trim()
                            ? { namespace: namespace.trim() }
                            : {}),
                        },
                      ],
                      expires_in_days: durations[duration],
                    },
                  });
                  setCreated(r.data);
                  toast("Token created", "Copy the secret now");
                });
                void ok;
              }}
            >
              {action.busy ? "Creating…" : "Create token once"}
            </Button>
          </>
        )
      }
    >
      <Failure error={action.error} />
      {created ? (
        <>
          <p>
            {created.name} · expires {when(created.expires_at)}
          </p>
          <div className="row end">
            <Input
              label="New token secret — copy now"
              type="password"
              mono
              autoComplete="off"
              readOnly
              value={created.token}
              onFocus={(e: { currentTarget: HTMLInputElement }) =>
                e.currentTarget.select()
              }
            />
            <IconButton
              icon={copied ? "check" : "copy"}
              label="Copy token secret"
              onClick={async () => {
                try {
                  await navigator.clipboard.writeText(created.token);
                  setCopied(true);
                } catch {
                  /* clipboard unavailable: select the field and copy */
                }
              }}
            />
          </div>
          <Alert tone="warn" title="Shown once">
            If you lose it, revoke the token and create a new one.
          </Alert>
        </>
      ) : attempted ? (
        <Alert tone="warn" title="A creation request was attempted">
          Its secret cannot be shown again. Create a separate token if you did
          not receive it.
        </Alert>
      ) : (
        <>
          <Input
            label="Name"
            value={name}
            maxLength={128}
            onChange={val(setName)}
            hint="For example: laptop or ci-runner."
          />
          <Select
            label="Feed"
            options={[
              { value: "", label: "All feeds" },
              ...feeds.items.map((f) => ({ value: f.id, label: f.slug })),
            ]}
            value={feed}
            onChange={val(setFeed)}
          />
          <Input
            label="Namespace (optional)"
            mono
            maxLength={200}
            value={namespace}
            onChange={val(setNamespace)}
            hint="Limit the token to one package namespace."
          />
          <ActionPicker value={actions} onChange={setActions} />
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
        </>
      )}
    </Modal>
  );
}
