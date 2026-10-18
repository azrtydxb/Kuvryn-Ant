import { useEffect, useState } from "react";
import { Client, requireETag } from "./api";
import { Field, Failure, Mutation, Reload, useResource } from "./ui";
import type {
  CreatePrincipal,
  IdentityPrincipal,
  IdentityRole,
  IdentityRoleGrant,
  IssueServiceToken,
  IssuedServiceToken,
} from "./generated";
const uuid =
  "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
const actions: IdentityRoleGrant["actions"] = [
  "read",
  "publish",
  "delete",
  "configure",
  "replicate",
  "admin",
];
function revision(n: number) {
  if (!Number.isSafeInteger(n) || n < 1)
    throw Error("Invalid revision. Refresh the list.");
  return requireETag(`"${n}"`);
}
function Next({
  after,
  next,
  setAfter,
}: {
  after: string;
  next?: string;
  setAfter: (s: string) => void;
}) {
  return (
    <div className="pager">
      <button disabled={!after} onClick={() => setAfter("")}>
        First page
      </button>
      <button disabled={!next} onClick={() => setAfter(next!)}>
        Next page
      </button>
    </div>
  );
}
function Actions({
  value,
  onChange,
}: {
  value: IdentityRoleGrant["actions"];
  onChange: (v: IdentityRoleGrant["actions"]) => void;
}) {
  return (
    <fieldset className="actions">
      <legend>Actions (select at least one)</legend>
      {actions.map((a) => (
        <label className="check" key={a}>
          <input
            type="checkbox"
            checked={value.includes(a)}
            onChange={(e) =>
              onChange(
                e.target.checked ? [...value, a] : value.filter((x) => x !== a),
              )
            }
          />
          {a}
        </label>
      ))}
    </fieldset>
  );
}
function GrantEditor({
  grants,
  onChange,
}: {
  grants: IdentityRoleGrant[];
  onChange: (g: IdentityRoleGrant[]) => void;
}) {
  const update = (i: number, g: IdentityRoleGrant) =>
    onChange(grants.map((v, n) => (n === i ? g : v)));
  return (
    <>
      {grants.map((g, i) => (
        <fieldset className="rule" key={i}>
          <legend>Grant {i + 1}</legend>
          <div className="grid">
            <Field label="Resource kind">
              <select
                value={g.kind}
                onChange={(e) =>
                  update(i, {
                    kind: e.target.value as IdentityRoleGrant["kind"],
                    effect: g.effect,
                    actions: g.actions,
                  })
                }
              >
                {["artifact", "feed", "source", "instance"].map((k) => (
                  <option key={k}>{k}</option>
                ))}
              </select>
            </Field>
            <Field label="Effect">
              <select
                value={g.effect}
                onChange={(e) =>
                  update(i, {
                    ...g,
                    effect: e.target.value as "allow" | "deny",
                  })
                }
              >
                <option>allow</option>
                <option>deny</option>
              </select>
            </Field>
          </div>
          {g.kind !== "instance" && (
            <>
              {(g.kind === "artifact" || g.kind === "feed") && (
                <Field label="Feed UUID (optional)">
                  <input
                    pattern={uuid}
                    value={g.feed_id ?? ""}
                    onChange={(e) =>
                      update(i, {
                        ...g,
                        feed_id: e.target.value || undefined,
                        source_id: undefined,
                      })
                    }
                  />
                </Field>
              )}
              {(g.kind === "source" || g.kind === "artifact") && (
                <Field label="Source UUID (optional)">
                  <input
                    pattern={uuid}
                    value={g.source_id ?? ""}
                    onChange={(e) =>
                      update(i, {
                        ...g,
                        source_id: e.target.value || undefined,
                        feed_id: undefined,
                      })
                    }
                  />
                </Field>
              )}
              {g.kind === "artifact" && (
                <Field label="Namespace (optional)">
                  <input
                    maxLength={200}
                    value={g.namespace ?? ""}
                    onChange={(e) =>
                      update(i, {
                        ...g,
                        namespace: e.target.value || undefined,
                      })
                    }
                  />
                </Field>
              )}
            </>
          )}
          <Actions
            value={g.actions}
            onChange={(a) => update(i, { ...g, actions: a })}
          />
          <button
            type="button"
            onClick={() => onChange(grants.filter((_, n) => n !== i))}
          >
            Remove grant {i + 1}
          </button>
        </fieldset>
      ))}
      <button
        type="button"
        disabled={grants.length >= 256}
        onClick={() =>
          onChange([
            ...grants,
            { kind: "artifact", effect: "allow", actions: ["read"] },
          ])
        }
      >
        Add grant
      </button>
      <p className="notice">
        Empty scope fields broaden a grant. Deny wins over allow. Instance admin
        grants enable identity administration; review changes before saving.
      </p>
    </>
  );
}
function RoleEditor({
  client,
  role,
  done,
}: {
  client: Client;
  role: IdentityRole;
  done: () => void;
}) {
  const [grants, setGrants] = useState(role.grants);
  return (
    <Mutation
      label="Replace role grants"
      run={async () => {
        if (grants.some((g) => !g.actions.length))
          throw Error("Every grant needs at least one action.");
        await client.call("setRoleGrants", {
          params: { id: role.id },
          etag: revision(role.revision),
          body: { grants },
        });
        done();
      }}
    >
      <GrantEditor grants={grants} onChange={setGrants} />
      <p>
        Saving replaces all grants for <strong>{role.name}</strong> at revision{" "}
        {role.revision}.
      </p>
    </Mutation>
  );
}
function Memberships({
  client,
  principal,
  done,
}: {
  client: Client;
  principal: IdentityPrincipal;
  done: () => void;
}) {
  const [ids, setIDs] = useState(principal.role_ids.join("\n"));
  return (
    <Mutation
      label="Replace role memberships"
      run={async () => {
        const role_ids = ids.split(/[\s,]+/).filter(Boolean);
        if (
          role_ids.length > 256 ||
          role_ids.some((s) => !new RegExp("^" + uuid + "$").test(s)) ||
          new Set(role_ids).size !== role_ids.length
        )
          throw Error("Enter up to 256 unique role UUIDs.");
        await client.call("setPrincipalRoles", {
          params: { id: principal.id },
          etag: revision(principal.revision),
          body: { role_ids },
        });
        done();
      }}
    >
      <Field
        label="Role UUIDs"
        hint="One per line. Copy IDs from Roles. Empty removes all memberships."
      >
        <textarea
          rows={4}
          value={ids}
          onChange={(e) => setIDs(e.target.value)}
        />
      </Field>
      <p>
        Saving replaces every role membership for{" "}
        <strong>{principal.subject}</strong> at revision {principal.revision}.
      </p>
    </Mutation>
  );
}
function Binding({
  client,
  principal,
}: {
  client: Client;
  principal: IdentityPrincipal;
}) {
  const [issuer, setIssuer] = useState("");
  const [subject, setSubject] = useState("");
  return (
    <Mutation
      label="Create identity binding"
      run={async () => {
        await client.call("createIdentityBinding", {
          body: { issuer, subject, principal_id: principal.id },
        });
        setIssuer("");
        setSubject("");
      }}
    >
      <p>
        Bind a verified provider identity to this explicit local user:{" "}
        <strong>{principal.subject}</strong> (<code>{principal.id}</code>). This
        does not create a user or assign roles.
      </p>
      <Field label="Provider issuer / identifier">
        <input
          required
          maxLength={2048}
          value={issuer}
          onChange={(e) => setIssuer(e.target.value)}
        />
      </Field>
      <Field label="Immutable provider subject">
        <input
          required
          maxLength={2048}
          value={subject}
          onChange={(e) => setSubject(e.target.value)}
        />
      </Field>
      <p className="muted">
        Use exact issuer and immutable subject values from the trusted provider,
        not an inferred email. Existing bindings cannot be silently reassigned.
        This API does not list bindings.
      </p>
    </Mutation>
  );
}
function TokenIssuer({
  client,
  principal,
}: {
  client: Client;
  principal: IdentityPrincipal;
}) {
  const [expires, setExpires] = useState("");
  const [scopes, setScopes] = useState<IssueServiceToken["scopes"]>([
    { actions: ["read"] },
  ]);
  const [issued, setIssued] = useState<IssuedServiceToken>();
  const [attempted, setAttempted] = useState(false);
  const [issuing, setIssuing] = useState(false);
  const [issueError, setIssueError] = useState<unknown>();
  useEffect(() => {
    if (!issued) return;
    const timer = setTimeout(() => setIssued(undefined), 60000);
    return () => clearTimeout(timer);
  }, [issued]);
  return (
    <>
      <p>
        Issue a scoped token for <strong>{principal.subject}</strong>. The
        server restricts effective permissions. The secret is displayed once and
        cleared from this view after one minute or on navigation.
      </p>
      <Failure error={issueError} />
      {issuing && <p role="status">Issuing token…</p>}
      {issued && (
        <div className="notice">
          <p>
            Token ID: <code>{issued.id}</code>
          </p>
          <Field label="New token secret — copy now">
            <input
              type="password"
              autoComplete="off"
              readOnly
              value={issued.secret}
              onFocus={(e) => e.currentTarget.select()}
            />
          </Field>
          <p>Expires {new Date(issued.expires_at).toLocaleString()}</p>
          <button onClick={() => setIssued(undefined)}>
            Clear displayed secret
          </button>
        </div>
      )}
      {attempted ? (
        <p className="notice">
          An issuance request was attempted. Its secret cannot be replayed, even
          if the response was lost.{" "}
          <button
            disabled={issuing}
            onClick={() => {
              setAttempted(false);
              setIssued(undefined);
              setIssueError(undefined);
            }}
          >
            Start a separate token issuance
          </button>
        </p>
      ) : (
        <Mutation
          label="Issue token once"
          run={async () => {
            const date = new Date(expires);
            if (
              !Number.isFinite(date.getTime()) ||
              date.getTime() <= Date.now() ||
              date.getTime() > Date.now() + 365 * 86400000
            )
              throw Error("Choose a future expiry within one year.");
            if (scopes.some((s) => !s.actions.length))
              throw Error("Each token scope needs at least one action.");
            setAttempted(true);
            setIssuing(true);
            setIssueError(undefined);
            try {
              const result = await client.call("issueServiceToken", {
                params: { id: principal.id },
                body: { scopes, expires_at: date.toISOString() },
              });
              setIssued(result.data);
            } catch (e) {
              setIssueError(e);
            } finally {
              setIssuing(false);
            }
          }}
        >
          <Field label="Token expiry (local time)">
            <input
              type="datetime-local"
              required
              value={expires}
              onChange={(e) => setExpires(e.target.value)}
            />
          </Field>
          {scopes.map((s, i) => (
            <fieldset className="rule" key={i}>
              <legend>Token scope {i + 1}</legend>
              <Field label="Feed UUID (optional)">
                <input
                  pattern={uuid}
                  value={s.feed_id ?? ""}
                  onChange={(e) =>
                    setScopes(
                      scopes.map((v, n) =>
                        n === i
                          ? { ...v, feed_id: e.target.value || undefined }
                          : v,
                      ),
                    )
                  }
                />
              </Field>
              <Field label="Namespace (optional)">
                <input
                  maxLength={200}
                  value={s.namespace ?? ""}
                  onChange={(e) =>
                    setScopes(
                      scopes.map((v, n) =>
                        n === i
                          ? { ...v, namespace: e.target.value || undefined }
                          : v,
                      ),
                    )
                  }
                />
              </Field>
              <Actions
                value={s.actions}
                onChange={(a) =>
                  setScopes(
                    scopes.map((v, n) => (n === i ? { ...v, actions: a } : v)),
                  )
                }
              />
              <button
                type="button"
                disabled={scopes.length === 1}
                onClick={() => setScopes(scopes.filter((_, n) => n !== i))}
              >
                Remove scope {i + 1}
              </button>
            </fieldset>
          ))}
          <button
            type="button"
            disabled={scopes.length >= 64}
            onClick={() => setScopes([...scopes, { actions: ["read"] }])}
          >
            Add token scope
          </button>
          <p className="notice">
            Empty scope fields apply broadly. Token issuance is not replayable
            and is never automatically retried.
          </p>
        </Mutation>
      )}
    </>
  );
}
function Revoke({ client }: { client: Client }) {
  const [id, setID] = useState("");
  const [confirm, setConfirm] = useState(false);
  return (
    <section className="card">
      <h2>Revoke a service token</h2>
      <p>
        Token listing is not exposed by this contract. Enter the token ID
        recorded at issuance.
      </p>
      <Mutation
        label="Revoke token"
        run={async () => {
          await client.call("revokeServiceToken", { params: { id } });
          setID("");
          setConfirm(false);
        }}
      >
        <Field label="Token ID to revoke">
          <input
            required
            pattern={uuid}
            value={id}
            onChange={(e) => {
              setID(e.target.value);
              setConfirm(false);
            }}
          />
        </Field>
        <label className="check">
          <input
            type="checkbox"
            required
            checked={confirm}
            onChange={(e) => setConfirm(e.target.checked)}
          />
          Revoke this token immediately
        </label>
      </Mutation>
    </section>
  );
}
export function Identity({ client }: { client: Client }) {
  const [section, setSection] = useState<"principals" | "roles" | "tokens">(
    "principals",
  );
  const [after, setAfter] = useState("");
  const [subject, setSubject] = useState("");
  const [kind, setKind] = useState<CreatePrincipal["kind"]>("user");
  const [name, setName] = useState("");
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
        query: { limit: "50", ...(after ? { after } : {}) },
      }),
    [client, after],
  );
  return (
    <>
      <p className="eyebrow">ACCESS CONTROL</p>
      <h1>Identity administration</h1>
      <p>
        These operations require instance admin permission. User identity
        bindings and role memberships are explicit; signing in does not
        provision access.
      </p>
      <nav className="tabs" aria-label="Identity sections">
        {(["principals", "roles", "tokens"] as const).map((s) => (
          <button
            key={s}
            aria-current={section === s ? "page" : undefined}
            onClick={() => {
              setAfter("");
              setSection(s);
            }}
          >
            {s[0].toUpperCase() + s.slice(1)}
          </button>
        ))}
      </nav>
      {section === "principals" ? (
        <>
          <section className="card">
            <h2>Create principal</h2>
            <Mutation
              label="Create principal"
              run={async () => {
                await client.call("createPrincipal", {
                  body: { subject, kind },
                });
                setSubject("");
                principals.reload();
              }}
            >
              <Field label="Local principal subject">
                <input
                  required
                  maxLength={200}
                  value={subject}
                  onChange={(e) => setSubject(e.target.value)}
                />
              </Field>
              <Field label="Principal kind">
                <select
                  value={kind}
                  onChange={(e) =>
                    setKind(e.target.value as CreatePrincipal["kind"])
                  }
                >
                  <option>user</option>
                  <option>service</option>
                </select>
              </Field>
            </Mutation>
          </section>
          <Reload {...principals} />
          <Failure error={principals.error} />
          {principals.loading && <p role="status">Loading principals…</p>}
          {principals.value && (
            <>
              {!principals.value.data.items.length && (
                <p>No principals found.</p>
              )}
              {principals.value.data.items.map((p) => (
                <section className="card" key={p.id + ":" + p.revision}>
                  <h2>{p.subject}</h2>
                  <p>
                    {p.kind} · {p.disabled ? "Disabled" : "Enabled"} · Revision{" "}
                    {p.revision}
                  </p>
                  <code>{p.id}</code>
                  <details>
                    <summary>Role memberships</summary>
                    <Memberships
                      client={client}
                      principal={p}
                      done={principals.reload}
                    />
                  </details>
                  {p.kind === "user" && !p.disabled && (
                    <details>
                      <summary>Bind provider identity</summary>
                      <Binding client={client} principal={p} />
                    </details>
                  )}
                  {p.kind === "service" && !p.disabled && (
                    <details>
                      <summary>Issue scoped service token</summary>
                      <TokenIssuer client={client} principal={p} />
                    </details>
                  )}
                </section>
              ))}
              <Next
                after={after}
                next={principals.value.data.next_after}
                setAfter={setAfter}
              />
            </>
          )}
        </>
      ) : section === "roles" ? (
        <>
          <section className="card">
            <h2>Create role</h2>
            <Mutation
              label="Create role"
              run={async () => {
                await client.call("createRole", { body: { name } });
                setName("");
                roles.reload();
              }}
            >
              <Field label="Role name">
                <input
                  required
                  maxLength={128}
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                />
              </Field>
            </Mutation>
          </section>
          <Reload {...roles} />
          <Failure error={roles.error} />
          {roles.loading && <p role="status">Loading roles…</p>}
          {roles.value && (
            <>
              {!roles.value.data.items.length && <p>No roles found.</p>}
              {roles.value.data.items.map((r) => (
                <section className="card" key={r.id + ":" + r.revision}>
                  <h2>{r.name}</h2>
                  <code>{r.id}</code>
                  <p>
                    Revision {r.revision} · {r.grants.length} grants
                  </p>
                  <details>
                    <summary>Display and edit grants</summary>
                    <RoleEditor client={client} role={r} done={roles.reload} />
                  </details>
                </section>
              ))}
              <Next
                after={after}
                next={roles.value.data.next_after}
                setAfter={setAfter}
              />
            </>
          )}
        </>
      ) : (
        <Revoke client={client} />
      )}
    </>
  );
}
