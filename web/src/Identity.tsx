import { useEffect, useMemo, useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllFeeds } from "./data";
import { getProviders, type Providers } from "./auth";
import { isUnavailable, useOptionalCall } from "./gaps";
import type {
  EffectiveAction,
  IdentityBindingRecord,
  CreatePrincipal,
  IdentityPrincipal,
  IdentityRole,
  IdentityRoleGrant,
} from "./generated";
import { GrantEditor } from "./Grants";
import { TokenIssuer, TokensTab } from "./Tokens";
import {
  Alert,
  Avatar,
  Badge,
  Button,
  Checkbox,
  Drawer,
  DropdownMenu,
  EmptyState,
  Icon,
  IconButton,
  Input,
  Modal,
  Select,
  Table,
  Tabs,
} from "./ds/bundle";
import {
  Card,
  Copyable,
  Failure,
  Loading,
  Mono,
  Needs,
  PageHeading,
  Pager,
  Toolbar,
  Unavailable,
  chk,
  useAction,
  useResource,
  useToast,
  val,
} from "./ui";

function revision(n: number) {
  if (!Number.isSafeInteger(n) || n < 1)
    throw Error("Invalid revision. Refresh the list.");
  return requireETag(`"${n}"`);
}

export function Identity({ client }: { client: Client }) {
  const [tab, setTab] = useState("users");
  const [after, setAfter] = useState("");
  const [creating, setCreating] = useState<CreatePrincipal["kind"]>();
  const [creatingRole, setCreatingRole] = useState(false);
  const [issuing, setIssuing] = useState(false);
  const [binding, setBinding] = useState(false);
  const principals = useResource(
    () =>
      client.call("listPrincipals", {
        query: { limit: "50", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  const roles = useResource(
    () =>
      client.call("listRoles", {
        query: { limit: "200" },
      }),
    [client],
  );
  const [roleAfter, setRoleAfter] = useState("");
  const roleList = useResource(
    () =>
      client.call("listRoles", {
        query: { limit: "50", ...(roleAfter ? { after: roleAfter } : {}) },
      }),
    [client, roleAfter],
  );
  const items = principals.value?.data.items ?? [];
  const users = items.filter((p) => p.kind !== "service");
  const services = items.filter((p) => p.kind === "service");
  const allRoles = roles.value?.data.items ?? [];
  const changeTab = (t: string) => {
    setTab(t);
    setAfter("");
  };
  return (
    <>
      <PageHeading
        eyebrow="Identity & access"
        title="Access"
        text="People, service principals, roles and tokens. Explicit deny wins; token scopes intersect role grants. Signing in never provisions access."
      >
        {tab === "users" && (
          <Button icon="user-plus" onClick={() => setCreating("user")}>
            Add user
          </Button>
        )}
        {tab === "services" && (
          <Button icon="bot" onClick={() => setCreating("service")}>
            Add service principal
          </Button>
        )}
        {tab === "roles" && (
          <Button icon="plus" onClick={() => setCreatingRole(true)}>
            Create role
          </Button>
        )}
        {tab === "tokens" && (
          <Button icon="key-round" onClick={() => setIssuing(true)}>
            Issue token
          </Button>
        )}
        {tab === "providers" && (
          <Button icon="link" onClick={() => setBinding(true)}>
            Bind identity
          </Button>
        )}
      </PageHeading>
      <Tabs
        value={tab}
        onChange={changeTab}
        className="spaced-tabs"
        items={[
          { id: "users", label: "Users", count: users.length },
          {
            id: "services",
            label: "Service principals",
            count: services.length,
          },
          { id: "roles", label: "Roles" },
          { id: "tokens", label: "Tokens" },
          { id: "providers", label: "Sign-in providers" },
        ]}
      />
      <p className="muted small">
        These operations require instance admin permission.
      </p>
      {(tab === "users" || tab === "services") && (
        <PrincipalsTab
          client={client}
          kind={tab === "users" ? "user" : "service"}
          rows={tab === "users" ? users : services}
          roles={allRoles}
          state={principals}
          after={after}
          setAfter={setAfter}
        />
      )}
      {tab === "roles" && (
        <RolesTab
          client={client}
          state={roleList}
          after={roleAfter}
          setAfter={setRoleAfter}
        />
      )}
      {tab === "tokens" && <TokensTab client={client} principals={items} />}
      {tab === "providers" && (
        <ProvidersTab client={client} principals={items} />
      )}
      {creating && (
        <CreatePrincipalModal
          client={client}
          kind={creating}
          onClose={() => setCreating(undefined)}
          done={() => {
            setCreating(undefined);
            principals.reload();
          }}
        />
      )}
      {creatingRole && (
        <CreateRoleModal
          client={client}
          onClose={() => setCreatingRole(false)}
          done={() => {
            setCreatingRole(false);
            roleList.reload();
            roles.reload();
          }}
        />
      )}
      {issuing && (
        <TokenIssuer
          client={client}
          principals={items}
          onClose={() => setIssuing(false)}
        />
      )}
      {binding && (
        <BindModal
          client={client}
          principals={users}
          onClose={() => setBinding(false)}
        />
      )}
    </>
  );
}

function PrincipalsTab({
  client,
  kind,
  rows,
  roles,
  state,
  after,
  setAfter,
}: {
  client: Client;
  kind: "user" | "service";
  rows: IdentityPrincipal[];
  roles: IdentityRole[];
  state: ReturnType<
    typeof useResource<{ items: IdentityPrincipal[]; next_after?: string }>
  >;
  after: string;
  setAfter: (s: string) => void;
}) {
  const [filter, setFilter] = useState("");
  const [editing, setEditing] = useState<IdentityPrincipal>();
  const [binding, setBinding] = useState<IdentityPrincipal>();
  const [effective, setEffective] = useState<IdentityPrincipal>();
  const [token, setToken] = useState<IdentityPrincipal>();
  const shown = rows.filter((p) =>
    (p.subject + p.id).toLowerCase().includes(filter.trim().toLowerCase()),
  );
  const roleName = (id: string) => roles.find((r) => r.id === id)?.name ?? id;
  return (
    <>
      <Toolbar right={<Needs>effective permissions view</Needs>}>
        <Input
          icon="search"
          size="sm"
          aria-label="Filter by name or ID"
          placeholder="Name or ID"
          value={filter}
          onChange={val(setFilter)}
          className="w240"
        />
        <Button
          size="sm"
          variant="secondary"
          icon="refresh-cw"
          disabled={state.loading}
          onClick={state.reload}
        >
          Refresh
        </Button>
      </Toolbar>
      <Failure error={state.error} />
      {state.loading && (
        <Loading what={kind === "user" ? "users" : "service principals"} />
      )}
      {state.value &&
        (shown.length ? (
          <Table
            columns={[
              {
                key: "subject",
                label: kind === "user" ? "User" : "Service principal",
                render: (p: IdentityPrincipal) => (
                  <span className="row">
                    {kind === "user" ? (
                      <Avatar name={p.subject} size={28} />
                    ) : (
                      <Icon name="bot" size={16} />
                    )}
                    <span>
                      {p.subject}
                      <small>{p.id}</small>
                    </span>
                  </span>
                ),
              },
              {
                key: "roles",
                label: "Roles",
                render: (p: IdentityPrincipal) =>
                  p.role_ids.length ? (
                    <span className="row tight wrap">
                      {p.role_ids.map((id) => (
                        <Badge key={id} tone="neutral">
                          {roleName(id)}
                        </Badge>
                      ))}
                    </span>
                  ) : (
                    <span className="muted">None</span>
                  ),
              },
              {
                key: "kind",
                label: "Kind",
                render: (p: IdentityPrincipal) =>
                  p.kind === "emergency" ? (
                    <Badge tone="warn">emergency</Badge>
                  ) : (
                    p.kind
                  ),
              },
              { key: "revision", label: "Revision", align: "right" },
              {
                key: "disabled",
                label: "Status",
                render: (p: IdentityPrincipal) => (
                  <Badge tone={p.disabled ? "neutral" : "good"} dot>
                    {p.disabled ? "Disabled" : "Enabled"}
                  </Badge>
                ),
              },
              {
                key: "x",
                label: "",
                align: "right",
                render: (p: IdentityPrincipal) => (
                  <DropdownMenu
                    align="end"
                    trigger={
                      <IconButton
                        icon="ellipsis"
                        label={`Actions for ${p.subject}`}
                      />
                    }
                    items={[
                      {
                        label: "Edit roles",
                        icon: "shield",
                        onSelect: () => setEditing(p),
                      },
                      {
                        label: "Effective permissions",
                        icon: "list-checks",
                        onSelect: () => setEffective(p),
                      },
                      ...(p.kind === "user" && !p.disabled
                        ? [
                            {
                              label: "Bind provider identity",
                              icon: "link",
                              onSelect: () => setBinding(p),
                            },
                          ]
                        : []),
                      ...(p.kind === "service" && !p.disabled
                        ? [
                            {
                              label: "Issue token",
                              icon: "key-round",
                              onSelect: () => setToken(p),
                            },
                          ]
                        : []),
                    ]}
                  />
                ),
              },
            ]}
            rows={shown}
          />
        ) : (
          <EmptyState
            icon={kind === "user" ? "users" : "bot"}
            title={`No ${kind === "user" ? "users" : "service principals"} found`}
            description="Create one to grant access explicitly."
          />
        ))}
      {state.value && (
        <Pager
          cursor={after}
          next={state.value.data.next_after}
          set={setAfter}
        />
      )}
      {editing && (
        <MembershipsModal
          client={client}
          principal={editing}
          roles={roles}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            state.reload();
          }}
        />
      )}
      {binding && (
        <BindModal
          client={client}
          principals={[binding]}
          onClose={() => setBinding(undefined)}
        />
      )}
      {token && (
        <TokenIssuer
          client={client}
          principals={[token]}
          initial={token.id}
          onClose={() => setToken(undefined)}
        />
      )}
      {effective && (
        <EffectiveDrawer
          client={client}
          principal={effective}
          onClose={() => setEffective(undefined)}
        />
      )}
    </>
  );
}

function CreatePrincipalModal({
  client,
  kind,
  onClose,
  done,
}: {
  client: Client;
  kind: CreatePrincipal["kind"];
  onClose: () => void;
  done: () => void;
}) {
  const [subject, setSubject] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title={kind === "user" ? "Add user" : "Add service principal"}
      description="Creates a local principal with no roles. Bind a provider identity or issue tokens afterwards."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-principal"
            type="submit"
            disabled={action.busy || !subject.trim()}
          >
            {action.busy ? "Creating…" : "Create principal"}
          </Button>
        </>
      }
    >
      <form
        id="create-principal"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("createPrincipal", { body: { subject, kind } }),
          );
          if (ok) {
            toast("Principal created", subject);
            done();
          }
        }}
      >
        <Input
          label="Local principal subject"
          mono
          required
          maxLength={200}
          value={subject}
          onChange={val(setSubject)}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function MembershipsModal({
  client,
  principal,
  roles,
  onClose,
  done,
}: {
  client: Client;
  principal: IdentityPrincipal;
  roles: IdentityRole[];
  onClose: () => void;
  done: () => void;
}) {
  const [ids, setIDs] = useState(principal.role_ids);
  const action = useAction();
  const toast = useToast();
  const unknown = ids.filter((id) => !roles.some((r) => r.id === id));
  return (
    <Modal
      title={`Roles for ${principal.subject}`}
      description={`Saving replaces every role membership at revision ${principal.revision}.`}
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("setPrincipalRoles", {
                  params: { id: principal.id },
                  etag: revision(principal.revision),
                  body: { role_ids: ids },
                }),
              );
              if (ok) {
                toast("Role memberships replaced", principal.subject);
                done();
              }
            }}
          >
            {action.busy ? "Saving…" : "Replace role memberships"}
          </Button>
        </>
      }
    >
      {roles.length ? (
        roles.map((r) => (
          <Checkbox
            key={r.id}
            label={r.name}
            hint={`${r.grants.length} grant${r.grants.length === 1 ? "" : "s"}`}
            checked={ids.includes(r.id)}
            onChange={chk((on) =>
              setIDs(on ? [...ids, r.id] : ids.filter((x) => x !== r.id)),
            )}
          />
        ))
      ) : (
        <p className="muted">
          No roles exist yet. Create one on the Roles tab.
        </p>
      )}
      {unknown.length > 0 && (
        <p className="muted small">
          {unknown.length} membership(s) refer to roles not listed here and are
          kept unless you remove them below.
        </p>
      )}
      {unknown.map((id) => (
        <Checkbox
          key={id}
          label={<Mono>{id}</Mono>}
          checked
          onChange={() => setIDs(ids.filter((x) => x !== id))}
        />
      ))}
      <Failure error={action.error} />
    </Modal>
  );
}

function BindModal({
  client,
  principals,
  onClose,
}: {
  client: Client;
  principals: IdentityPrincipal[];
  onClose: () => void;
}) {
  const eligible = principals.filter((p) => p.kind === "user" && !p.disabled);
  const [principal, setPrincipal] = useState(eligible[0]?.id ?? "");
  const [issuer, setIssuer] = useState("");
  const [subject, setSubject] = useState("");
  const action = useAction();
  const toast = useToast();
  const target = eligible.find((p) => p.id === principal);
  return (
    <Modal
      title="Bind identity"
      description="Bind a verified provider identity to an explicit local user. This does not create a user or assign roles."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="bind-identity"
            type="submit"
            disabled={action.busy || !principal}
          >
            {action.busy ? "Binding…" : "Create identity binding"}
          </Button>
        </>
      }
    >
      <form
        id="bind-identity"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("createIdentityBinding", {
              body: { issuer, subject, principal_id: principal },
            }),
          );
          if (ok) {
            toast("Identity bound", target?.subject);
            setIssuer("");
            setSubject("");
            onClose();
          }
        }}
      >
        <Select
          label="Local user"
          options={
            eligible.length
              ? eligible.map((p) => ({ value: p.id, label: p.subject }))
              : [{ value: "", label: "No enabled users" }]
          }
          value={principal}
          onChange={val(setPrincipal)}
        />
        <Input
          label="Provider issuer / identifier"
          mono
          required
          maxLength={2048}
          value={issuer}
          onChange={val(setIssuer)}
        />
        <Input
          label="Immutable provider subject"
          mono
          required
          maxLength={2048}
          value={subject}
          onChange={val(setSubject)}
          hint="Use exact issuer and immutable subject values from the trusted provider, not an inferred email. Existing bindings cannot be silently reassigned."
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function EffectiveDrawer({
  client,
  principal,
  onClose,
}: {
  client: Client;
  principal: IdentityPrincipal;
  onClose: () => void;
}) {
  const feeds = useAllFeeds(client);
  const [resource, setResource] = useState("instance");
  const r = useOptionalCall(client, "getEffectivePermissions", {
    query: { principal: principal.id, resource },
  });
  return (
    <Drawer
      open
      title={`Effective permissions · ${principal.subject}`}
      description="From role grants. Machine-token scopes narrow a specific token further and are not applied."
      onClose={onClose}
      width={520}
      footer={<Button onClick={onClose}>Done</Button>}
    >
      <Select
        label="Resource"
        options={[
          { value: "instance", label: "Instance" },
          ...feeds.items.map((f) => ({
            value: `feed:${f.id}`,
            label: `Feed · ${f.slug}`,
          })),
        ]}
        value={resource}
        onChange={val(setResource)}
      />
      {r.state === "loading" && <Loading what="effective permissions" />}
      {r.state === "unavailable" && (
        <Unavailable what="Effective permissions" />
      )}
      {r.state === "error" && <Failure error={r.error} />}
      {r.state === "ready" && (
        <>
          {r.data.disabled && (
            <Alert tone="warn" title="Principal is disabled" />
          )}
          <Table
            rowKey="action"
            columns={[
              { key: "action", label: "Action", mono: true },
              {
                key: "allowed",
                label: "Result",
                render: (a: EffectiveAction) => (
                  <Badge tone={a.allowed ? "good" : "neutral"} dot>
                    {a.allowed ? "Allowed" : "Not allowed"}
                  </Badge>
                ),
              },
              {
                key: "via",
                label: "Roles",
                render: (a: EffectiveAction) =>
                  a.denied_by_roles.length
                    ? `denied by ${a.denied_by_roles.join(", ")}`
                    : a.via_roles.join(", ") || "—",
              },
            ]}
            rows={r.data.actions}
          />
        </>
      )}
    </Drawer>
  );
}

function RolesTab({
  client,
  state,
  after,
  setAfter,
}: {
  client: Client;
  state: ReturnType<
    typeof useResource<{ items: IdentityRole[]; next_after?: string }>
  >;
  after: string;
  setAfter: (s: string) => void;
}) {
  const [editing, setEditing] = useState<IdentityRole>();
  return (
    <>
      <Failure error={state.error} />
      {state.loading && <Loading what="roles" />}
      {state.value &&
        (state.value.data.items.length ? (
          <Table
            onRowClick={(r: IdentityRole) => setEditing(r)}
            columns={[
              {
                key: "name",
                label: "Role",
                render: (r: IdentityRole) => (
                  <>
                    {r.name}
                    <small>{r.id}</small>
                  </>
                ),
              },
              {
                key: "grants",
                label: "Grants",
                render: (r: IdentityRole) => <Mono>{summarize(r.grants)}</Mono>,
              },
              { key: "revision", label: "Revision", align: "right" },
            ]}
            rows={state.value.data.items}
          />
        ) : (
          <EmptyState
            icon="shield"
            title="No roles found"
            description="Create a role, then add structured grants to it."
          />
        ))}
      {state.value && (
        <Pager
          cursor={after}
          next={state.value.data.next_after}
          set={setAfter}
        />
      )}
      {editing && (
        <RoleDrawer
          key={editing.id + ":" + editing.revision}
          client={client}
          role={editing}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            state.reload();
          }}
        />
      )}
    </>
  );
}
function summarize(grants: IdentityRoleGrant[]) {
  if (!grants.length) return "No grants";
  return grants
    .slice(0, 3)
    .map(
      (g) =>
        `${g.effect === "deny" ? "deny " : ""}${g.actions.join(",")} · ${g.kind}`,
    )
    .join("; ")
    .concat(grants.length > 3 ? ` +${grants.length - 3} more` : "");
}

function RoleDrawer({
  client,
  role,
  onClose,
  done,
}: {
  client: Client;
  role: IdentityRole;
  onClose: () => void;
  done: () => void;
}) {
  const [grants, setGrants] = useState(role.grants);
  const action = useAction();
  const toast = useToast();
  return (
    <Drawer
      open
      title={role.name}
      description={<Copyable text={role.id} />}
      onClose={onClose}
      width={620}
      footer={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy}
            onClick={async () => {
              const ok = await action.run(async () => {
                if (grants.some((g) => !g.actions.length))
                  throw Error("Every grant needs at least one action.");
                await client.call("setRoleGrants", {
                  params: { id: role.id },
                  etag: revision(role.revision),
                  body: { grants },
                });
              });
              if (ok) {
                toast("Role grants replaced", role.name);
                done();
              }
            }}
          >
            {action.busy ? "Saving…" : "Replace role grants"}
          </Button>
        </>
      }
    >
      <p className="muted small">
        Saving replaces all grants for <strong>{role.name}</strong> at revision{" "}
        {role.revision}.
      </p>
      <GrantEditor client={client} grants={grants} onChange={setGrants} />
      <Failure error={action.error} />
    </Drawer>
  );
}

function CreateRoleModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [name, setName] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Create role"
      description="A new role has no grants. Add structured grants after creating it."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="create-role"
            type="submit"
            disabled={action.busy || !name.trim()}
          >
            {action.busy ? "Creating…" : "Create role"}
          </Button>
        </>
      }
    >
      <form
        id="create-role"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("createRole", { body: { name } }),
          );
          if (ok) {
            toast("Role created", name);
            done();
          }
        }}
      >
        <Input
          label="Role name"
          required
          maxLength={128}
          value={name}
          onChange={val(setName)}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function ProvidersTab({
  client,
  principals,
}: {
  client: Client;
  principals: IdentityPrincipal[];
}) {
  const [providers, setProviders] = useState<Providers | null>();
  useEffect(() => {
    let active = true;
    getProviders()
      .then((p) => active && setProviders(p))
      .catch(() => active && setProviders(null));
    return () => {
      active = false;
    };
  }, []);
  const bindings = useOptionalCall(client, "listIdentityBindings", {
    query: { limit: "200" },
  });
  const [removing, setRemoving] = useState<IdentityBindingRecord>();
  const name = useMemo(
    () => (id: string) => principals.find((p) => p.id === id)?.subject ?? id,
    [principals],
  );
  const cards: [string, keyof Providers][] = [
    ["OIDC", "oidc"],
    ["SAML", "saml"],
    ["LDAP", "ldap"],
  ];
  return (
    <>
      {providers === undefined && <Loading what="sign-in providers" />}
      {providers === null && (
        <Alert
          tone="warn"
          title="Provider availability could not be determined"
        >
          Configured providers are discovered from the authentication service.
        </Alert>
      )}
      {providers && (
        <div className="grid-auto grid-auto--280">
          {cards.map(([label, key]) => (
            <Card
              key={key}
              title={label}
              actions={
                <Badge tone={providers[key] ? "good" : "neutral"} dot>
                  {providers[key] ? "Configured" : "Not configured"}
                </Badge>
              }
            >
              <p className="muted small">
                Configured by mounted file. Login never creates administrators;
                bind subjects to existing users.
              </p>
            </Card>
          ))}
        </div>
      )}
      <h2 className="h3 spaced-top">Identity bindings</h2>
      {bindings.state === "loading" && <Loading what="bindings" />}
      {bindings.state === "error" && <Failure error={bindings.error} />}
      {bindings.state === "unavailable" && (
        <>
          <Unavailable what="Listing and removing identity bindings" compact />
          <p className="muted small">
            You can still create bindings with Bind identity. Existing bindings
            cannot be listed or deleted yet.
          </p>
        </>
      )}
      {bindings.state === "ready" && (
        <Table
          columns={[
            { key: "issuer", label: "Issuer", mono: true },
            { key: "subject", label: "Subject", mono: true },
            {
              key: "principal_id",
              label: "Local user",
              render: (b: IdentityBindingRecord) => name(b.principal_id),
            },
            {
              key: "x",
              label: "",
              align: "right",
              render: (b: IdentityBindingRecord) => (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => setRemoving(b)}
                >
                  Remove
                </Button>
              ),
            },
          ]}
          rows={bindings.data.items}
        />
      )}
      {removing && (
        <RemoveBinding
          client={client}
          binding={removing}
          onClose={() => setRemoving(undefined)}
          done={() => {
            setRemoving(undefined);
            bindings.reload();
          }}
        />
      )}
    </>
  );
}

function RemoveBinding({
  client,
  binding,
  onClose,
  done,
}: {
  client: Client;
  binding: IdentityBindingRecord;
  onClose: () => void;
  done: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const [unavailable, setUnavailable] = useState(false);
  return (
    <Modal
      title="Remove identity binding"
      description="The provider identity can no longer sign in as this user."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            disabled={action.busy || unavailable}
            onClick={async () => {
              let gone = false;
              const ok = await action.run(async () => {
                try {
                  await client.call("deleteIdentityBinding", {
                    params: { id: binding.id },
                  });
                } catch (e) {
                  if (!isUnavailable(e)) throw e;
                  gone = true;
                  setUnavailable(true);
                }
              });
              if (ok && !gone) {
                toast("Binding removed");
                done();
              }
            }}
          >
            Remove binding
          </Button>
        </>
      }
    >
      <p>
        <Mono>{binding.issuer}</Mono> · <Mono>{binding.subject}</Mono>
      </p>
      {unavailable && <Unavailable what="Removing identity bindings" compact />}
      <Failure error={action.error} />
    </Modal>
  );
}
