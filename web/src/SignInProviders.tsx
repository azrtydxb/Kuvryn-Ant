import { useState } from "react";
import { requireETag, type Client } from "./api";
import type {
  CertificateStatus,
  IdentityProvider,
  IdentityProviderConfig,
  IdentityProviderInput,
  IdentityProviderList,
  IdentityProviderTest,
  MappingPreview,
  SamlMetadataImport,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  DropdownMenu,
  IconButton,
  Input,
  Modal,
  Select,
  Switch,
  Table,
  Textarea,
} from "./ds/bundle";
import {
  Card,
  Copyable,
  Failure,
  Mono,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

type Kind = IdentityProvider["kind"];
const kindLabel: Record<Kind, string> = {
  oidc: "OpenID Connect",
  saml: "SAML 2.0",
  ldap: "LDAP / Active Directory",
};
const rev = (n: number) => requireETag(`"${n}"`);

function certTone(s: CertificateStatus["status"]) {
  return s === "ok" ? "good" : s === "expiring" ? "warn" : "bad";
}

export function Certificates({ items }: { items: CertificateStatus[] }) {
  if (!items.length) return null;
  return (
    <Table
      rowKey="sha256"
      columns={[
        { key: "subject", label: "Certificate", mono: true },
        {
          key: "use",
          label: "Use",
          render: (c: CertificateStatus) => c.use ?? "signing",
        },
        {
          key: "not_after",
          label: "Expires",
          render: (c: CertificateStatus) => when(c.not_after),
        },
        {
          key: "status",
          label: "Status",
          render: (c: CertificateStatus) => (
            <Badge tone={certTone(c.status)} dot>
              {c.status === "ok" ? "Valid" : c.status.replace("_", " ")}
              {c.status === "expiring" ? ` · ${c.days_left} d` : ""}
            </Badge>
          ),
        },
      ]}
      rows={items}
    />
  );
}

export function TestResult({ result }: { result: IdentityProviderTest }) {
  return (
    <div className="stack" aria-live="polite">
      <Alert
        tone={result.ok ? "good" : "bad"}
        title={result.ok ? "All checks passed" : "Some checks failed"}
      />
      <Table
        rowKey="name"
        columns={[
          {
            key: "ok",
            label: "",
            render: (c: { ok: boolean }) => (
              <Badge tone={c.ok ? "good" : "bad"} dot>
                {c.ok ? "Pass" : "Fail"}
              </Badge>
            ),
          },
          { key: "name", label: "Check", mono: true },
          { key: "detail", label: "Detail" },
        ]}
        rows={result.checks}
      />
      {result.tls_mode && (
        <p className="muted small">
          Transport:{" "}
          {result.tls_mode === "ldaps" ? "LDAPS (implicit TLS)" : "StartTLS"}
        </p>
      )}
      {result.clock_skew_seconds != null && (
        <p className="muted small">
          Clock difference from the provider: {result.clock_skew_seconds} s
          {Math.abs(result.clock_skew_seconds) > 30 &&
            " (over the 30 s tolerance; fix NTP)"}
        </p>
      )}
      {result.warnings.map((w) => (
        <Alert key={w} tone="warn">
          {w}
        </Alert>
      ))}
      <Certificates items={result.certificates} />
    </div>
  );
}

export function PreviewResult({ preview }: { preview: MappingPreview }) {
  return (
    <div className="stack" aria-live="polite">
      <Alert
        tone={preview.would_sign_in ? "good" : "bad"}
        title={
          preview.would_sign_in
            ? "This identity could sign in"
            : "This identity would be refused"
        }
      />
      {preview.problems.map((p) => (
        <Alert key={p} tone="bad">
          {p}
        </Alert>
      ))}
      {preview.warnings.map((p) => (
        <Alert key={p} tone="warn">
          {p}
        </Alert>
      ))}
      <dl className="props">
        <dt>Issuer</dt>
        <dd>
          <Mono>{preview.identity.issuer || "—"}</Mono>
        </dd>
        <dt>Subject</dt>
        <dd>
          <Mono>{preview.identity.subject || "—"}</Mono>
        </dd>
        <dt>Groups</dt>
        <dd>
          {preview.identity.groups.length
            ? preview.identity.groups.join(", ")
            : "none"}
        </dd>
        <dt>Bound local user</dt>
        <dd>
          {preview.binding.found ? (
            <Mono>{preview.binding.principal_id ?? ""}</Mono>
          ) : (
            "not bound"
          )}
        </dd>
        <dt>Effective roles</dt>
        <dd>
          {preview.binding.roles.length
            ? preview.binding.roles.join(", ")
            : "none"}
        </dd>
        {preview.proposed_roles.length > 0 && (
          <>
            <dt>Mapping proposes</dt>
            <dd>{preview.proposed_roles.join(", ")} (informational)</dd>
          </>
        )}
      </dl>
      {preview.clock_skew_seconds != null && (
        <p className="muted small">
          Clock difference: {preview.clock_skew_seconds} s
        </p>
      )}
      <p className="muted small">
        {preview.signature_verified
          ? "Resolved from the directory."
          : "Signature not verified: this previews mapping only."}
      </p>
    </div>
  );
}

const lines = (s: string) =>
  s
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

function parseGroupRoles(s: string) {
  return lines(s).map((l) => {
    const [group, roles = ""] = l.split("=");
    return {
      group_id: group.trim(),
      role_ids: roles
        .split(",")
        .map((r) => r.trim())
        .filter(Boolean),
    };
  });
}

function parseClaimRoles(s: string) {
  return parseGroupRoles(s).map(({ group_id, role_ids }) => ({
    value: group_id,
    role_ids,
  }));
}

export function ProviderEditor({
  client,
  kind,
  existing,
  origin,
  onClose,
  done,
}: {
  client: Client;
  kind: Kind;
  existing?: IdentityProvider;
  origin: string;
  onClose: () => void;
  done: () => void;
}) {
  const c = existing?.config ?? {};
  const [name, setName] = useState(existing?.name ?? "");
  const [enabled, setEnabled] = useState(existing?.enabled ?? false);
  const [secret, setSecret] = useState("");
  const [issuer, setIssuer] = useState(c.issuer ?? "");
  const [clientId, setClientId] = useState(c.client_id ?? "");
  const [endpoints, setEndpoints] = useState(
    (c.allowed_endpoints ?? []).join("\n"),
  );
  const [groupsClaim, setGroupsClaim] = useState(c.groups_claim ?? "");
  const [rolesClaim, setRolesClaim] = useState(c.roles_claim ?? "");
  const [scopes, setScopes] = useState((c.scopes ?? []).join(" "));
  const [claimRoles, setClaimRoles] = useState(
    (c.claim_roles ?? [])
      .map((m) => `${m.value}=${m.role_ids.join(",")}`)
      .join("\n"),
  );
  const [entityId, setEntityId] = useState(
    c.entity_id ?? (origin ? `${origin}/auth/saml/metadata` : ""),
  );
  const [idpEntity, setIdpEntity] = useState(c.idp_entity_id ?? "");
  const [metadata, setMetadata] = useState(c.idp_metadata_xml ?? "");
  const [groupsAttr, setGroupsAttr] = useState(c.groups_attribute ?? "");
  const [emailAttr, setEmailAttr] = useState(c.email_attribute ?? "");
  const [rotate, setRotate] = useState(false);
  const [providerId, setProviderId] = useState(c.provider_id ?? "");
  const [url, setUrl] = useState(c.url ?? "ldaps://");
  const [bindDn, setBindDn] = useState(c.bind_dn ?? "");
  const [caPem, setCaPem] = useState(c.ca_pem ?? "");
  const [userBase, setUserBase] = useState(c.user_base_dn ?? "");
  const [groupBase, setGroupBase] = useState(c.group_base_dn ?? "");
  const [loginAttr, setLoginAttr] = useState(c.login_attribute ?? "uid");
  const [userIdAttr, setUserIdAttr] = useState(
    c.user_id_attribute ?? "entryUUID",
  );
  const [groupIdAttr, setGroupIdAttr] = useState(
    c.group_id_attribute ?? "entryUUID",
  );
  const [userClass, setUserClass] = useState(c.user_class ?? "inetOrgPerson");
  const [groupClass, setGroupClass] = useState(c.group_class ?? "groupOfNames");
  const [memberAttr, setMemberAttr] = useState(c.member_attribute ?? "member");
  const [groupRoles, setGroupRoles] = useState(
    (c.group_roles ?? [])
      .map((g) => `${g.group_id}=${g.role_ids.join(",")}`)
      .join("\n"),
  );
  const [result, setResult] = useState<IdentityProviderTest>();
  const [imported, setImported] = useState<SamlMetadataImport>();
  const save = useAction();
  const test = useAction();
  const importing = useAction();
  const toast = useToast();

  const config = (): IdentityProviderConfig =>
    kind === "oidc"
      ? {
          issuer,
          client_id: clientId,
          allowed_endpoints: lines(endpoints),
          ...(groupsClaim ? { groups_claim: groupsClaim } : {}),
          ...(rolesClaim ? { roles_claim: rolesClaim } : {}),
          ...(lines(claimRoles).length
            ? { claim_roles: parseClaimRoles(claimRoles) }
            : {}),
          ...(scopes.trim() ? { scopes: scopes.trim().split(/\s+/) } : {}),
        }
      : kind === "saml"
        ? {
            entity_id: entityId,
            idp_entity_id: idpEntity,
            idp_metadata_xml: metadata,
            ...(groupsAttr ? { groups_attribute: groupsAttr } : {}),
            ...(emailAttr ? { email_attribute: emailAttr } : {}),
          }
        : {
            provider_id: providerId,
            url,
            bind_dn: bindDn,
            ...(caPem ? { ca_pem: caPem } : {}),
            user_base_dn: userBase,
            group_base_dn: groupBase,
            login_attribute: loginAttr,
            user_id_attribute: userIdAttr,
            group_id_attribute: groupIdAttr,
            user_class: userClass,
            group_class: groupClass,
            member_attribute: memberAttr,
            group_roles: parseGroupRoles(groupRoles),
          };

  return (
    <Modal
      title={`${existing ? "Edit" : "Add"} ${kindLabel[kind]} provider`}
      description="Secrets are encrypted at rest and never shown again. Saving an enabled provider builds it first; a broken configuration is rejected and nothing changes."
      onClose={onClose}
      size="lg"
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="secondary"
            disabled={test.busy}
            onClick={async () => {
              setResult(undefined);
              await test.run(async () => {
                const r = await client.call("testIdentityProvider", {
                  body: {
                    ...(existing ? { id: existing.id } : {}),
                    kind,
                    config: config(),
                    ...(secret ? { secret } : {}),
                  },
                });
                setResult(r.data);
              });
            }}
          >
            {test.busy ? "Testing…" : "Test connection"}
          </Button>
          <Button form="provider-editor" type="submit" disabled={save.busy}>
            {save.busy ? "Saving…" : "Save provider"}
          </Button>
        </>
      }
    >
      <form
        id="provider-editor"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await save.run(async () => {
            const body: IdentityProviderInput = {
              kind,
              name,
              enabled,
              config: config(),
              ...(secret ? { secret } : {}),
              ...(rotate ? { rotate_sp_certificate: true } : {}),
            };
            if (existing)
              await client.call("updateIdentityProvider", {
                params: { id: existing.id },
                etag: rev(existing.revision),
                body,
              });
            else await client.call("createIdentityProvider", { body });
            setSecret("");
          });
          if (ok) {
            toast(
              "Provider saved",
              enabled ? "Active without restart" : "Saved disabled",
            );
            done();
          }
        }}
      >
        <Input
          label="Name"
          required
          maxLength={64}
          value={name}
          onChange={val(setName)}
        />
        {kind === "oidc" && (
          <>
            <Input
              label="Issuer URL"
              required
              mono
              value={issuer}
              onChange={val(setIssuer)}
              hint="Exactly as the provider advertises it, https only."
            />
            <Input
              label="Client ID"
              required
              mono
              value={clientId}
              onChange={val(setClientId)}
            />
            <Input
              label="Client secret"
              type="password"
              autoComplete="off"
              value={secret}
              onChange={val(setSecret)}
              hint={
                existing?.has_secret
                  ? "A secret is stored. Leave empty to keep it."
                  : "Leave empty for a public client."
              }
            />
            <Textarea
              label="Approved endpoints"
              mono
              rows={3}
              value={endpoints}
              onChange={val(setEndpoints)}
              hint="Authorization, token and JWKS URLs the server may call, one per line. Run the test to get the discovered values."
            />
            {result?.suggested_allowed_endpoints &&
              result.suggested_allowed_endpoints.length > 0 && (
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() =>
                    setEndpoints(result.suggested_allowed_endpoints!.join("\n"))
                  }
                >
                  Use discovered endpoints
                </Button>
              )}
            <Input
              label="Groups claim (preview only)"
              value={groupsClaim}
              onChange={val(setGroupsClaim)}
              hint="Default: groups"
            />
            <Input
              label="Roles claim"
              mono
              value={rolesClaim}
              onChange={val(setRolesClaim)}
              hint="ID-token claim to map to roles, e.g. groups or urn:zitadel:iam:org:project:<id>:roles."
            />
            <Textarea
              label="Claim to role mapping"
              mono
              rows={3}
              value={claimRoles}
              onChange={val(setClaimRoles)}
              hint="One per line: claim value=role ID[,role ID]. Users with no explicit binding are created at first login; explicit bindings always win."
            />
            <Input
              label="Extra scopes"
              mono
              value={scopes}
              onChange={val(setScopes)}
              hint="Space separated, besides openid."
            />
          </>
        )}
        {kind === "saml" && (
          <>
            <Input
              label="Service provider entity ID"
              required
              mono
              value={entityId}
              onChange={val(setEntityId)}
            />
            <Textarea
              label="IdP metadata XML"
              mono
              rows={6}
              required
              value={metadata}
              onChange={val(setMetadata)}
              hint="Paste the metadata document from your IdP. It is never fetched remotely."
            />
            <div className="row">
              <Button
                size="sm"
                variant="secondary"
                disabled={importing.busy || !metadata}
                onClick={async () => {
                  await importing.run(async () => {
                    const r = await client.call("importSamlMetadata", {
                      body: { xml: metadata },
                    });
                    setImported(r.data);
                    if (r.data.entity_id) setIdpEntity(r.data.entity_id);
                  });
                }}
              >
                {importing.busy ? "Reading…" : "Read metadata"}
              </Button>
              <Input
                label="IdP entity ID"
                required
                mono
                value={idpEntity}
                onChange={val(setIdpEntity)}
              />
            </div>
            <Failure error={importing.error} />
            {imported && (
              <div className="stack">
                {imported.errors.map((m) => (
                  <Alert key={m} tone="bad">
                    {m}
                  </Alert>
                ))}
                {imported.warnings.map((m) => (
                  <Alert key={m} tone="warn">
                    {m}
                  </Alert>
                ))}
                <p className="muted small">
                  {imported.sso_endpoints.length} sign-on endpoint(s);{" "}
                  {imported.certificates.length} certificate(s).
                </p>
                <Certificates items={imported.certificates} />
              </div>
            )}
            <Input
              label="Groups attribute"
              value={groupsAttr}
              onChange={val(setGroupsAttr)}
              hint="Attribute carrying group names (preview only)."
            />
            <Input
              label="Email attribute"
              value={emailAttr}
              onChange={val(setEmailAttr)}
            />
            {existing && (
              <>
                <Certificates items={existing.certificates} />
                <Switch
                  label="Rotate service provider key pair on save"
                  labelPosition="end"
                  checked={rotate}
                  onChange={(e: { target: { checked: boolean } }) =>
                    setRotate(e.target.checked)
                  }
                />
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={async () => {
                    const text = await client.exportText(
                      `/api/v1/identity-providers/${existing.id}/metadata`,
                      {},
                    );
                    const link = document.createElement("a");
                    link.href = URL.createObjectURL(
                      new Blob([text], {
                        type: "application/samlmetadata+xml",
                      }),
                    );
                    link.download = "ant-sp-metadata.xml";
                    link.click();
                    URL.revokeObjectURL(link.href);
                  }}
                >
                  Download SP metadata
                </Button>
              </>
            )}
          </>
        )}
        {kind === "ldap" && (
          <>
            <Input
              label="Provider ID"
              required
              mono
              value={providerId}
              onChange={val(setProviderId)}
              hint="A stable https URL that names this directory; used as the identity issuer."
            />
            <Input
              label="Server URL"
              required
              mono
              value={url}
              onChange={val(setUrl)}
              hint="ldaps:// uses implicit TLS; ldap:// always upgrades with StartTLS. Certificates are always verified."
            />
            <Input
              label="Bind DN"
              required
              mono
              value={bindDn}
              onChange={val(setBindDn)}
            />
            <Input
              label="Bind password"
              type="password"
              autoComplete="off"
              value={secret}
              required={!existing?.has_secret}
              onChange={val(setSecret)}
              hint={
                existing?.has_secret
                  ? "A password is stored. Leave empty to keep it."
                  : undefined
              }
            />
            <Textarea
              label="CA certificate (PEM, optional)"
              mono
              rows={3}
              value={caPem}
              onChange={val(setCaPem)}
            />
            <Input
              label="User base DN"
              required
              mono
              value={userBase}
              onChange={val(setUserBase)}
            />
            <Input
              label="Group base DN"
              required
              mono
              value={groupBase}
              onChange={val(setGroupBase)}
            />
            <div className="cols">
              <Input
                label="Login attribute"
                mono
                value={loginAttr}
                onChange={val(setLoginAttr)}
              />
              <Select
                label="User ID attribute"
                options={[
                  { value: "entryUUID", label: "entryUUID" },
                  { value: "objectGUID", label: "objectGUID" },
                ]}
                value={userIdAttr}
                onChange={val(setUserIdAttr)}
              />
              <Select
                label="Group ID attribute"
                options={[
                  { value: "entryUUID", label: "entryUUID" },
                  { value: "objectGUID", label: "objectGUID" },
                ]}
                value={groupIdAttr}
                onChange={val(setGroupIdAttr)}
              />
            </div>
            <div className="cols">
              <Input
                label="User class"
                mono
                value={userClass}
                onChange={val(setUserClass)}
              />
              <Input
                label="Group class"
                mono
                value={groupClass}
                onChange={val(setGroupClass)}
              />
              <Input
                label="Member attribute"
                mono
                value={memberAttr}
                onChange={val(setMemberAttr)}
              />
            </div>
            <Textarea
              label="Group to role proposals"
              mono
              rows={3}
              value={groupRoles}
              onChange={val(setGroupRoles)}
              hint="One per line: base64url-group-id=role-id,role-id. Informational: access still comes from identity bindings."
            />
          </>
        )}
        <Switch
          label="Enable (replaces the enabled provider of this type, takes effect immediately)"
          labelPosition="end"
          checked={enabled}
          onChange={(e: { target: { checked: boolean } }) =>
            setEnabled(e.target.checked)
          }
        />
        <Failure error={save.error} />
      </form>
      <Failure error={test.error} />
      {result && <TestResult result={result} />}
    </Modal>
  );
}

function PreviewModal({
  client,
  provider,
  onClose,
}: {
  client: Client;
  provider: IdentityProvider;
  onClose: () => void;
}) {
  const [sample, setSample] = useState("");
  const [login, setLogin] = useState("");
  const [preview, setPreview] = useState<MappingPreview>();
  const run = useAction();
  const ldap = provider.kind === "ldap";
  return (
    <Modal
      title={`Mapping preview · ${provider.name}`}
      description={
        ldap
          ? "Looks the user up in the directory (no password) and shows the resulting identity, groups and roles."
          : "Paste an ID token or SAML response. It is decoded in memory, signature-unverified, and never stored or audited."
      }
      onClose={onClose}
      size="lg"
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Close
          </Button>
          <Button
            disabled={run.busy || (ldap ? !login : !sample)}
            onClick={async () => {
              setPreview(undefined);
              await run.run(async () => {
                const r = await client.call("previewIdentityMapping", {
                  params: { id: provider.id },
                  body: ldap ? { login } : { sample },
                });
                setPreview(r.data);
                setSample("");
              });
            }}
          >
            {run.busy ? "Resolving…" : "Preview"}
          </Button>
        </>
      }
    >
      {ldap ? (
        <Input
          label="LDAP login"
          value={login}
          onChange={val(setLogin)}
          autoComplete="off"
        />
      ) : (
        <Textarea
          label={
            provider.kind === "oidc"
              ? "ID token (JWT)"
              : "SAML response (base64 or XML)"
          }
          mono
          rows={6}
          value={sample}
          onChange={val(setSample)}
          autoComplete="off"
          spellCheck={false}
        />
      )}
      <Failure error={run.error} />
      {preview && <PreviewResult preview={preview} />}
    </Modal>
  );
}

function statusBadge(p: IdentityProvider) {
  if (p.shadowed_by_file)
    return (
      <Badge tone="warn" dot>
        Shadowed by file
      </Badge>
    );
  if (p.active)
    return (
      <Badge tone="good" dot>
        Active
      </Badge>
    );
  if (p.enabled && p.last_error)
    return (
      <Badge tone="bad" dot>
        Error
      </Badge>
    );
  if (p.enabled)
    return (
      <Badge tone="warn" dot>
        Enabled, not active
      </Badge>
    );
  return <Badge dot>Disabled</Badge>;
}

function LocalAccountCard({
  client,
  data,
  reload,
}: {
  client: Client;
  data: IdentityProviderList;
  reload: () => void;
}) {
  const acct = data.local_account;
  const toast = useToast();
  const policy = useAction();
  const unlock = useAction();
  const totp = useAction();
  const [uri, setUri] = useState("");
  const [code, setCode] = useState("");
  const locked =
    acct.locked_until && Date.parse(acct.locked_until) > Date.now();
  return (
    <Card
      title="Emergency administrator (break-glass)"
      actions={
        <Badge tone={acct.exists ? "good" : "warn"} dot>
          {acct.exists ? "Present" : "Not created"}
        </Badge>
      }
    >
      <div className="stack">
        {!acct.exists && (
          <Alert tone="warn" title="No break-glass account">
            Create one on the host with <Mono>ant admin break-glass</Mono>. It
            stays usable if SSO is misconfigured or down.
          </Alert>
        )}
        {acct.exists && (
          <p className="muted small">
            Username <Mono>{acct.username}</Mono> · two-factor{" "}
            {acct.totp_enabled ? "on" : "off"} · last sign-in{" "}
            {acct.last_login_at ? when(acct.last_login_at) : "never"}. Reset or
            recover it on the host with <Mono>ant admin break-glass</Mono>{" "}
            (needs the root keyring).
          </p>
        )}
        {locked && (
          <Alert tone="bad" title={`Locked until ${when(acct.locked_until)}`}>
            <Button
              size="sm"
              variant="secondary"
              disabled={unlock.busy}
              onClick={async () => {
                if (
                  await unlock.run(() =>
                    client.extra("POST", "/api/v1/local-account/unlock"),
                  )
                ) {
                  toast("Account unlocked");
                  reload();
                }
              }}
            >
              Unlock now
            </Button>
          </Alert>
        )}
        <Switch
          label="Require SSO: disable local password sign-in for everyone except the break-glass account"
          labelPosition="end"
          checked={data.sso_mandatory}
          disabled={policy.busy}
          onChange={async (e: { target: { checked: boolean } }) => {
            const next = e.target.checked;
            if (
              await policy.run(() =>
                client.call("putIdentityPolicy", {
                  body: { sso_mandatory: next },
                }),
              )
            ) {
              toast(next ? "SSO is now mandatory" : "Local sign-in allowed");
              reload();
            }
          }}
        />
        <Failure error={policy.error} />
        {acct.exists && !acct.totp_enabled && !uri && (
          <Button
            size="sm"
            variant="secondary"
            disabled={totp.busy}
            onClick={async () => {
              await totp.run(async () => {
                const r = await client.call("beginLocalTotp");
                setUri(r.data.otpauth_uri);
              });
            }}
          >
            Set up two-factor (TOTP)
          </Button>
        )}
        {uri && (
          <form
            className="stack"
            onSubmit={async (e) => {
              e.preventDefault();
              if (
                await totp.run(() =>
                  client.extra("POST", "/api/v1/local-account/totp/confirm", {
                    body: { code },
                  }),
                )
              ) {
                setUri("");
                setCode("");
                toast("Two-factor enabled");
                reload();
              }
            }}
          >
            <p className="muted small">
              Add this to an authenticator app, then enter the 6-digit code.
            </p>
            <Copyable text={uri} label="otpauth URI" />
            <Input
              label="Code"
              inputMode="numeric"
              pattern="[0-9]{6}"
              maxLength={6}
              required
              value={code}
              onChange={val(setCode)}
              autoComplete="one-time-code"
            />
            <Button type="submit" size="sm" disabled={totp.busy}>
              Enable
            </Button>
          </form>
        )}
        {acct.totp_enabled && (
          <Button
            size="sm"
            variant="ghost"
            disabled={totp.busy}
            onClick={async () => {
              if (
                await totp.run(() =>
                  client.extra("DELETE", "/api/v1/local-account/totp"),
                )
              ) {
                toast("Two-factor removed");
                reload();
              }
            }}
          >
            Remove two-factor
          </Button>
        )}
        <Failure error={totp.error} />
      </div>
    </Card>
  );
}

export function ProviderAdmin({
  client,
  data,
  reload,
}: {
  client: Client;
  data: IdentityProviderList;
  reload: () => void;
}) {
  const [editing, setEditing] = useState<{
    kind: Kind;
    existing?: IdentityProvider;
  }>();
  const [previewing, setPreviewing] = useState<IdentityProvider>();
  const [deleting, setDeleting] = useState<IdentityProvider>();
  const act = useAction();
  const del = useAction();
  const toast = useToast();
  return (
    <>
      <div className="row spread">
        <h2 className="h3">Sign-in providers</h2>
        <DropdownMenu
          align="end"
          trigger={<Button icon="plus">Add provider</Button>}
          items={(["oidc", "saml", "ldap"] as Kind[]).map((k) => ({
            label: kindLabel[k],
            onSelect: () => setEditing({ kind: k }),
          }))}
        />
      </div>
      <p className="muted small">
        Changes take effect immediately without a restart. Signing in never
        creates users or roles: bind each identity to an existing user.
      </p>
      {data.items.length === 0 && data.file_items.length === 0 && (
        <Alert tone="info" title="No sign-in providers configured">
          Add one above. Until then, sign in with an API token or the
          break-glass account.
        </Alert>
      )}
      <Failure error={act.error} />
      {data.file_items.length > 0 && (
        <Card title="Configured by file (read-only)">
          <Table
            rowKey="kind"
            columns={[
              {
                key: "kind",
                label: "Type",
                render: (f: { kind: Kind }) => kindLabel[f.kind],
              },
              { key: "label", label: "Identifier", mono: true },
              { key: "detail", label: "Detail" },
              {
                key: "x",
                label: "",
                render: () => <Badge tone="info">File</Badge>,
              },
            ]}
            rows={data.file_items}
          />
          <p className="muted small">
            Mounted files (ANT_*_CONFIG_FILE) take precedence over stored
            providers of the same type and cannot be edited here.
          </p>
        </Card>
      )}
      {data.items.length > 0 && (
        <Table
          rowKey="id"
          columns={[
            { key: "name", label: "Name" },
            {
              key: "kind",
              label: "Type",
              render: (p: IdentityProvider) => kindLabel[p.kind],
            },
            {
              key: "status",
              label: "Status",
              render: (p: IdentityProvider) => (
                <>
                  {statusBadge(p)}
                  {p.last_error && (
                    <small className="block muted">{p.last_error}</small>
                  )}
                </>
              ),
            },
            {
              key: "updated_at",
              label: "Updated",
              render: (p: IdentityProvider) => when(p.updated_at),
            },
            {
              key: "x",
              label: "",
              align: "right",
              render: (p: IdentityProvider) => (
                <DropdownMenu
                  align="end"
                  trigger={
                    <IconButton
                      icon="ellipsis"
                      label={`Actions for ${p.name}`}
                    />
                  }
                  items={[
                    {
                      label: "Edit",
                      icon: "pencil",
                      onSelect: () => setEditing({ kind: p.kind, existing: p }),
                    },
                    {
                      label: "Mapping preview",
                      icon: "search",
                      onSelect: () => setPreviewing(p),
                    },
                    {
                      label: p.enabled ? "Disable" : "Enable",
                      icon: p.enabled ? "pause" : "play",
                      onSelect: async () => {
                        const ok = await act.run(() =>
                          client.call(
                            p.enabled
                              ? "disableIdentityProvider"
                              : "enableIdentityProvider",
                            {
                              params: { id: p.id },
                              etag: rev(p.revision),
                            },
                          ),
                        );
                        if (ok) {
                          toast(
                            p.enabled
                              ? "Provider disabled"
                              : "Provider enabled",
                          );
                          reload();
                        }
                      },
                    },
                    {
                      label: "Delete",
                      icon: "trash-2",
                      onSelect: () => setDeleting(p),
                    },
                  ]}
                />
              ),
            },
          ]}
          rows={data.items}
        />
      )}
      <div className="spaced-top">
        <LocalAccountCard client={client} data={data} reload={reload} />
      </div>
      {editing && (
        <ProviderEditor
          client={client}
          kind={editing.kind}
          existing={editing.existing}
          origin={data.origin}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            reload();
          }}
        />
      )}
      {previewing && (
        <PreviewModal
          client={client}
          provider={previewing}
          onClose={() => setPreviewing(undefined)}
        />
      )}
      {deleting && (
        <Modal
          title="Delete provider"
          description="Its stored secret is erased and sign-in through it stops immediately."
          onClose={() => setDeleting(undefined)}
          actions={
            <>
              <Button
                variant="secondary"
                onClick={() => setDeleting(undefined)}
              >
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={del.busy}
                onClick={async () => {
                  const ok = await del.run(() =>
                    client.extra(
                      "DELETE",
                      `/api/v1/identity-providers/${deleting.id}`,
                      { etag: rev(deleting.revision) },
                    ),
                  );
                  if (ok) {
                    toast("Provider deleted");
                    setDeleting(undefined);
                    reload();
                  }
                }}
              >
                Delete provider
              </Button>
            </>
          }
        >
          <p>
            {deleting.name} · {kindLabel[deleting.kind]}
          </p>
          <Failure error={del.error} />
        </Modal>
      )}
    </>
  );
}
