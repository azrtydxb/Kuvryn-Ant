import { useState, type ReactNode } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import { FederationPolicy } from "./FederationPolicy";
import { FederationReplication } from "./FederationReplication";
import { FederationAuthority } from "./FederationAuthority";
import { FederationAudit } from "./FederationAudit";
import type {
  FederationIssuedInvitation,
  FederationMember,
  FederationInvitation,
  FederationStatus,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Drawer,
  EmptyState,
  Input,
  Modal,
  PropertyList,
  Table,
  Tabs,
} from "./ds/bundle";
import {
  Card,
  Copyable,
  Failure,
  Loading,
  Mono,
  PageHeading,
  SectionHead,
  Unavailable,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

type ActionState = ReturnType<typeof useAction>;
const healthTone: Record<string, string> = {
  ok: "good",
  degraded: "warn",
  disabled: "neutral",
};
const statusTone: Record<string, string> = {
  active: "good",
  suspended: "warn",
  removed: "bad",
};
const namePattern = /^[a-z0-9][a-z0-9-]{0,62}$/;

function age(seconds?: number) {
  if (seconds == null) return "—";
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

function labelText(labels?: object) {
  const entries = Object.entries((labels ?? {}) as Record<string, string>);
  return entries.length ? entries.map(([k, v]) => `${k}=${v}`).join(", ") : "—";
}

function parseLabels(text: string): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const part of text.split(/[\n,]+/)) {
    const item = part.trim();
    if (!item) continue;
    const [k, ...rest] = item.split("=");
    if (!k || !rest.length) return undefined;
    out[k.trim()] = rest.join("=").trim();
  }
  return out;
}

type Secret = { title: string; text: ReactNode; items: [string, string][] };

type FederationTab =
  "members" | "policy" | "replication" | "authority" | "audit";

export function FederationPage({ client }: { client: Client }) {
  const status = useOptionalCall(client, "getFederationStatus");
  const members = useOptionalCall(client, "listFederationMembers");
  const st = status.state === "ready" ? status.data : undefined;
  const authority = st?.state === "active" && st.role === "authority";
  const [dialog, setDialog] = useState<
    "found" | "join" | "invite" | "leave" | undefined
  >();
  const [open, setOpen] = useState<string>();
  const [tab, setTab] = useState<FederationTab>("members");
  const [secret, setSecret] = useState<Secret>();
  const [invitesVersion, setInvitesVersion] = useState(0);
  const reload = () => {
    status.reload();
    members.reload();
    setInvitesVersion((v) => v + 1);
  };
  const list = members.state === "ready" ? members.data.items : [];
  const current = list.find((m) => m.instance_id === open);
  const canStart =
    !st || st.state === "none" || st.state === "left" || st.state === "removed";
  const active = st?.state === "active";
  const shown: FederationTab = active ? tab : "members";
  return (
    <>
      <PageHeading
        eyebrow="Instances"
        title="Federation"
        text="Group independently operable instances under signed, versioned membership. Serving, publishing and replication never depend on the federation."
      >
        <Button variant="secondary" icon="refresh-cw" onClick={reload}>
          Refresh
        </Button>
        {st && canStart && (
          <>
            <Button
              variant="secondary"
              icon="log-in"
              onClick={() => setDialog("join")}
            >
              Join federation
            </Button>
            <Button icon="plus" onClick={() => setDialog("found")}>
              Found federation
            </Button>
          </>
        )}
        {authority && (
          <Button icon="user-plus" onClick={() => setDialog("invite")}>
            Invite member
          </Button>
        )}
        {st?.state === "active" && st.role === "member" && (
          <Button
            variant="danger"
            icon="log-out"
            onClick={() => setDialog("leave")}
          >
            Leave
          </Button>
        )}
      </PageHeading>
      {status.state === "loading" && <Loading what="federation status" />}
      {status.state === "unavailable" && <Unavailable what="Federation" />}
      {status.state === "error" && <Failure error={status.error} />}
      {st && (
        <>
          <StatusCard st={st} />
          {st.state !== "none" && (
            <>
              {active && (
                <Tabs
                  value={shown}
                  onChange={(v: string) => setTab(v as FederationTab)}
                  className="spaced-tabs"
                  items={[
                    { id: "members", label: "Members", count: list.length },
                    { id: "policy", label: "Policy" },
                    { id: "replication", label: "Replication" },
                    { id: "authority", label: "Authority" },
                    { id: "audit", label: "Audit" },
                  ]}
                />
              )}
              {shown === "members" && (
                <>
                  <SectionHead title="Members" count={list.length} />
                  {members.state === "loading" && <Loading what="members" />}
                  {members.state === "error" && (
                    <Failure error={members.error} />
                  )}
                  {members.state === "ready" &&
                    (list.length ? (
                      <Table
                        onRowClick={(m: FederationMember) =>
                          setOpen(m.instance_id)
                        }
                        columns={[
                          {
                            key: "name",
                            label: "Member",
                            render: (m: FederationMember) => (
                              <span className="row tight">
                                {m.name}
                                {m.authority && (
                                  <Badge tone="info">authority</Badge>
                                )}
                                {m.self && <Badge>this instance</Badge>}
                              </span>
                            ),
                          },
                          {
                            key: "status",
                            label: "Status",
                            render: (m: FederationMember) => (
                              <Badge tone={statusTone[m.status]}>
                                {m.status}
                              </Badge>
                            ),
                          },
                          {
                            key: "federation_url",
                            label: "Federation endpoint",
                            mono: true,
                          },
                          {
                            key: "labels",
                            label: "Labels",
                            render: (m: FederationMember) =>
                              labelText(m.labels),
                          },
                          {
                            key: "rtt",
                            label: "RTT",
                            align: "right",
                            render: (m: FederationMember) =>
                              m.self
                                ? "—"
                                : m.rtt_ms != null
                                  ? `${m.rtt_ms} ms`
                                  : "—",
                          },
                          {
                            key: "seen",
                            label: "Last seen",
                            render: (m: FederationMember) =>
                              m.self ? "—" : when(m.last_seen_at),
                          },
                        ]}
                        rows={list}
                      />
                    ) : (
                      <p className="muted">No members applied yet.</p>
                    ))}
                  {authority && (
                    <Invitations
                      key={invitesVersion}
                      client={client}
                      changed={reload}
                    />
                  )}
                </>
              )}
              {active && shown === "policy" && (
                <FederationPolicy client={client} st={st} />
              )}
              {active && shown === "replication" && (
                <FederationReplication client={client} st={st} members={list} />
              )}
              {active && shown === "authority" && (
                <FederationAuthority
                  client={client}
                  st={st}
                  members={list}
                  changed={reload}
                />
              )}
              {active && shown === "audit" && (
                <FederationAudit client={client} />
              )}
            </>
          )}
        </>
      )}
      {current && (
        <MemberDrawer
          client={client}
          member={current}
          authority={!!authority}
          onClose={() => setOpen(undefined)}
          changed={reload}
        />
      )}
      {dialog === "found" && (
        <FoundModal
          client={client}
          onClose={() => setDialog(undefined)}
          done={(s) => {
            setDialog(undefined);
            setSecret(s);
            reload();
          }}
        />
      )}
      {dialog === "join" && (
        <JoinModal
          client={client}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {dialog === "invite" && (
        <InviteModal
          client={client}
          onClose={() => setDialog(undefined)}
          done={(s) => {
            setDialog(undefined);
            setSecret(s);
            reload();
          }}
        />
      )}
      {dialog === "leave" && st && (
        <LeaveModal
          client={client}
          name={st.federation_name ?? ""}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {secret && (
        <Modal
          title={secret.title}
          onClose={() => setSecret(undefined)}
          width={640}
          actions={<Button onClick={() => setSecret(undefined)}>Done</Button>}
        >
          <Alert tone="warn" title="Shown once">
            {secret.text}
          </Alert>
          <PropertyList
            items={secret.items.map(([label, value]) => ({
              label,
              value: <Copyable text={value} label={"Copy " + label} />,
            }))}
          />
        </Modal>
      )}
    </>
  );
}

function StatusCard({ st }: { st: FederationStatus }) {
  if (st.state === "none")
    return (
      <>
        {!st.listener_configured && (
          <Alert tone="info" title="Federation listener not configured">
            Set ANT_FEDERATION_LISTEN and ANT_FEDERATION_PUBLIC_URL (https) to
            found or join a federation. Peer traffic uses this dedicated mTLS
            listener, never the public endpoint.
          </Alert>
        )}
        <EmptyState
          icon="network"
          title="Not federated"
          description="Found a federation to make this instance its authority, or join one with an invitation and the CA fingerprint from its authority."
        />
      </>
    );
  return (
    <>
      {st.health === "degraded" && (
        <Alert tone="warn" title="Federation degraded">
          {st.sync.last_error
            ? `Last sync failed: ${st.sync.last_error}. `
            : st.alerts?.length
              ? "See the federation alerts below. "
              : "The applied membership is older than 15 minutes. "}
          This instance keeps enforcing the last applied membership and policy
          and keeps serving.
        </Alert>
      )}
      {(st.state === "left" || st.state === "removed") && (
        <Alert
          tone="info"
          title={
            st.state === "left"
              ? "Left the federation"
              : "Removed from the federation"
          }
        >
          Content and local configuration are unchanged. Join again with a new
          invitation.
        </Alert>
      )}
      <Card
        title={
          <span className="row tight">
            {st.federation_name ?? "Federation"}
            <Badge tone={healthTone[st.health]}>{st.health}</Badge>
          </span>
        }
      >
        <PropertyList
          items={[
            { label: "State", value: st.state },
            {
              label: "Role",
              value: st.role === "authority" ? "Authority" : "Member",
            },
            {
              label: "Federation ID",
              value: st.federation_id ? (
                <Copyable text={st.federation_id} />
              ) : (
                "—"
              ),
            },
            {
              label: "Membership",
              value: st.membership
                ? `Generation ${st.membership.generation} · epoch ${st.authority_epoch ?? "—"} · confirmed ${age(st.membership.age_seconds)} ago`
                : "—",
            },
            {
              label: "Members",
              value: `${st.members.active} active · ${st.members.suspended} suspended · ${st.members.removed} removed`,
            },
            {
              label: "Federation endpoint",
              value: st.federation_url ? <Mono>{st.federation_url}</Mono> : "—",
            },
            {
              label: "Listener",
              value: st.listener_configured
                ? (st.listen_address ?? "configured")
                : "not configured",
            },
            {
              label: "CA fingerprint",
              value: st.ca_sha256 ? <Copyable text={st.ca_sha256} /> : "—",
            },
            {
              label: "Recovery key",
              value: st.recovery_key_fingerprint ? (
                <Mono>{st.recovery_key_fingerprint}</Mono>
              ) : (
                "—"
              ),
            },
            {
              label: "Certificate",
              value: st.certificate
                ? `${st.certificate.serial.slice(0, 16)}… · expires ${when(st.certificate.expires_at)}`
                : "—",
            },
            {
              label: "Last sync",
              value:
                st.role === "authority"
                  ? "Authority (source of membership)"
                  : st.sync.last_success_at
                    ? `${when(st.sync.last_success_at)}${st.sync.source ? " from " + st.sync.source : ""}`
                    : "never",
            },
          ]}
        />
      </Card>
    </>
  );
}

function Invitations({
  client,
  changed,
}: {
  client: Client;
  changed: () => void;
}) {
  const invites = useOptionalCall(client, "listFederationInvitations");
  const action = useAction();
  const toast = useToast();
  const items = invites.state === "ready" ? invites.data.items : [];
  return (
    <>
      <SectionHead title="Invitations" count={items.length} />
      <Failure error={action.error} />
      {invites.state === "loading" && <Loading what="invitations" />}
      {invites.state === "error" && <Failure error={invites.error} />}
      {invites.state === "ready" &&
        (items.length ? (
          <Table
            columns={[
              { key: "name", label: "Member name" },
              {
                key: "state",
                label: "State",
                render: (i: FederationInvitation) => (
                  <Badge
                    tone={
                      i.state === "pending"
                        ? "info"
                        : i.state === "consumed"
                          ? "good"
                          : "neutral"
                    }
                  >
                    {i.state}
                  </Badge>
                ),
              },
              {
                key: "expires_at",
                label: "Expires",
                render: (i: FederationInvitation) => when(i.expires_at),
              },
              {
                key: "actions",
                label: "",
                align: "right",
                render: (i: FederationInvitation) =>
                  i.state === "pending" ? (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={action.busy}
                      onClick={async () => {
                        if (
                          await action.run(() =>
                            client.call("revokeFederationInvitation", {
                              params: { id: i.id },
                            }),
                          )
                        ) {
                          toast("Invitation revoked");
                          changed();
                        }
                      }}
                    >
                      Revoke
                    </Button>
                  ) : null,
              },
            ]}
            rows={items}
          />
        ) : (
          <p className="muted">No invitations issued.</p>
        ))}
    </>
  );
}

function MemberDrawer({
  client,
  member,
  authority,
  onClose,
  changed,
}: {
  client: Client;
  member: FederationMember;
  authority: boolean;
  onClose: () => void;
  changed: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const [confirm, setConfirm] = useState(false);
  const manage = authority && !member.self && member.status !== "removed";
  const run = async (
    fn: () => Promise<unknown>,
    done: string,
  ): Promise<void> => {
    if (await action.run(fn)) {
      toast(done);
      setConfirm(false);
      changed();
    }
  };
  return (
    <Drawer
      open
      title={member.name}
      description={<Copyable text={member.instance_id} />}
      onClose={onClose}
      width={560}
      footer={
        manage ? (
          <>
            {member.status === "active" ? (
              <Button
                variant="secondary"
                icon="pause"
                disabled={action.busy}
                onClick={() =>
                  void run(
                    () =>
                      client.call("suspendFederationMember", {
                        params: { id: member.instance_id },
                      }),
                    "Member suspended",
                  )
                }
              >
                Suspend
              </Button>
            ) : (
              <Button
                variant="secondary"
                icon="play"
                disabled={action.busy}
                onClick={() =>
                  void run(
                    () =>
                      client.call("resumeFederationMember", {
                        params: { id: member.instance_id },
                      }),
                    "Member resumed",
                  )
                }
              >
                Resume
              </Button>
            )}
            <Button
              variant="danger"
              icon="trash-2"
              disabled={action.busy}
              onClick={() => setConfirm(true)}
            >
              Remove
            </Button>
          </>
        ) : undefined
      }
    >
      <Failure error={action.error} />
      <PropertyList
        items={[
          {
            label: "Status",
            value: (
              <Badge tone={statusTone[member.status]}>{member.status}</Badge>
            ),
          },
          {
            label: "Federation endpoint",
            value: <Mono>{member.federation_url}</Mono>,
          },
          {
            label: "Public URL",
            value: member.api_url ? <Mono>{member.api_url}</Mono> : "—",
          },
          { label: "Labels", value: labelText(member.labels) },
          { label: "Software", value: member.software_version || "—" },
          {
            label: "Protocol",
            value: `${member.protocol.min}–${member.protocol.max}`,
          },
          {
            label: "Certificate",
            value: member.cert_sha256 ? (
              <Copyable text={member.cert_sha256} />
            ) : (
              "—"
            ),
          },
          { label: "Joined", value: when(member.joined_at) },
          {
            label: "RTT / clock skew",
            value: member.self
              ? "—"
              : `${member.rtt_ms != null ? member.rtt_ms + " ms" : "—"} / ${member.skew_ms != null ? member.skew_ms + " ms" : "—"}`,
          },
          {
            label: "Membership generation",
            value: String(member.membership_generation),
          },
        ]}
      />
      {confirm && (
        <Modal
          title={`Remove ${member.name}?`}
          description="The member's certificate is revoked and every member refuses its federation calls once it applies the new membership. Its content is not deleted."
          onClose={() => setConfirm(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setConfirm(false)}>
                Cancel
              </Button>
              <Button
                variant="danger"
                disabled={action.busy}
                onClick={() =>
                  void run(
                    () =>
                      client.call("removeFederationMember", {
                        params: { id: member.instance_id },
                        query: { confirm: "true" },
                      }),
                    "Member removed",
                  )
                }
              >
                {action.busy ? "Removing…" : "Remove member"}
              </Button>
            </>
          }
        >
          <Failure error={action.error} />
        </Modal>
      )}
    </Drawer>
  );
}

function FormModal({
  title,
  description,
  submit,
  valid,
  action,
  onClose,
  onSubmit,
  danger,
  children,
}: {
  title: string;
  description: string;
  submit: string;
  valid: boolean;
  action: ActionState;
  onClose: () => void;
  onSubmit: () => void;
  danger?: boolean;
  children: ReactNode;
}) {
  return (
    <Modal
      title={title}
      description={description}
      onClose={onClose}
      width={600}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant={danger ? "danger" : undefined}
            disabled={action.busy || !valid}
            onClick={onSubmit}
          >
            {action.busy ? "Working…" : submit}
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      {children}
    </Modal>
  );
}

function FoundModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: (s: Secret) => void;
}) {
  const [name, setName] = useState("");
  const [member, setMember] = useState("");
  const action = useAction();
  const valid =
    namePattern.test(name) && (member === "" || namePattern.test(member));
  return (
    <FormModal
      title="Found a federation"
      description="This instance becomes the authority: it creates the federation CA, the authority signing key and an offline recovery key, and signs membership generation 1."
      submit="Found federation"
      valid={valid}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        let result: { recovery_key: string; ca_sha256: string } | undefined;
        void action
          .run(async () => {
            result = (
              await client.call("foundFederation", {
                body: { name, ...(member ? { member_name: member } : {}) },
              })
            ).data;
          })
          .then((ok) => {
            if (ok && result)
              done({
                title: "Federation founded",
                text: "Store the recovery key offline now. It is never stored on this instance and is needed for forced authority recovery and recovery-key rotation. Share the CA fingerprint with joining instances out of band.",
                items: [
                  ["Recovery key", result.recovery_key],
                  ["CA fingerprint", result.ca_sha256],
                ],
              });
          });
      }}
    >
      <Input
        label="Federation name"
        mono
        value={name}
        onChange={val(setName)}
        hint="Lowercase letters, digits and hyphens. Federated feed keys use it as a prefix."
      />
      <Input
        label="This instance's member name"
        mono
        value={member}
        onChange={val(setMember)}
        hint="Optional; defaults to the federation name."
      />
    </FormModal>
  );
}

function JoinModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [url, setUrl] = useState("");
  const [ca, setCa] = useState("");
  const [leaf, setLeaf] = useState("");
  const [invitation, setInvitation] = useState("");
  const action = useAction();
  const toast = useToast();
  const valid =
    url.startsWith("https://") &&
    /^[0-9a-fA-F]{64}$/.test(ca.trim()) &&
    (leaf.trim() === "" || /^[0-9a-fA-F]{64}$/.test(leaf.trim())) &&
    invitation.trim().length >= 40;
  return (
    <FormModal
      title="Join a federation"
      description="The connection to the authority is pinned to the CA fingerprint, so the system trust store is never used. The returned membership chain is verified before it is applied."
      submit="Join federation"
      valid={valid}
      action={action}
      onClose={onClose}
      onSubmit={() =>
        void action
          .run(() =>
            client.call("joinFederation", {
              body: {
                authority_url: url.trim(),
                ca_sha256: ca.trim(),
                ...(leaf.trim() ? { authority_cert_sha256: leaf.trim() } : {}),
                invitation: invitation.trim(),
              },
            }),
          )
          .then((ok) => {
            if (ok) {
              toast("Joined the federation");
              done();
            }
          })
      }
    >
      <Input
        label="Authority federation URL"
        mono
        value={url}
        onChange={val(setUrl)}
        hint="For example https://fed.ant.example:8444"
      />
      <Input
        label="CA fingerprint (SHA-256)"
        mono
        value={ca}
        onChange={val(setCa)}
        hint="64 hex characters, received out of band from the authority."
      />
      <Input
        label="Authority certificate fingerprint (SHA-256)"
        mono
        value={leaf}
        onChange={val(setLeaf)}
        hint="Recommended: from the invitation. Pins the authority certificate before the invitation is sent."
      />
      <Input
        label="Invitation"
        mono
        value={invitation}
        onChange={val(setInvitation)}
        hint="One-time secret issued by the authority for this member name."
      />
    </FormModal>
  );
}

function InviteModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: (s: Secret) => void;
}) {
  const [name, setName] = useState("");
  const [labels, setLabels] = useState("");
  const [hours, setHours] = useState("1");
  const action = useAction();
  const parsed = parseLabels(labels);
  const ttl = Math.round(Number(hours) * 3600);
  const valid =
    namePattern.test(name) && !!parsed && ttl >= 60 && ttl <= 604800;
  return (
    <FormModal
      title="Invite a member"
      description="The invitation is bound to a member name and can be used once. Its secret is shown once."
      submit="Create invitation"
      valid={valid}
      action={action}
      onClose={onClose}
      onSubmit={() => {
        let result: FederationIssuedInvitation | undefined;
        void action
          .run(async () => {
            result = (
              await client.call("createFederationInvitation", {
                body: { name, labels: parsed ?? {}, ttl_seconds: ttl },
              })
            ).data;
          })
          .then((ok) => {
            if (ok && result)
              done({
                title: `Invitation for ${result.name}`,
                text: `Give these values to the administrator of ${result.name}. The invitation expires ${when(result.expires_at)}.`,
                items: [
                  ["Authority URL", result.authority_url],
                  ["CA fingerprint", result.ca_sha256],
                  ["Authority certificate", result.authority_cert_sha256],
                  ["Invitation", result.secret],
                ],
              });
          });
      }}
    >
      <Input label="Member name" mono value={name} onChange={val(setName)} />
      <Input
        label="Labels"
        mono
        value={labels}
        onChange={val(setLabels)}
        hint="Optional key=value pairs separated by commas, for example site=kw, region=eu."
      />
      <Input
        label="Valid for (hours)"
        type="number"
        mono
        min={0.02}
        value={hours}
        onChange={val(setHours)}
      />
    </FormModal>
  );
}

function LeaveModal({
  client,
  name,
  onClose,
  done,
}: {
  client: Client;
  name: string;
  onClose: () => void;
  done: () => void;
}) {
  const [typed, setTyped] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <FormModal
      title="Leave the federation"
      description="The authority removes this member and revokes its certificate. Content and local configuration are kept."
      submit="Leave federation"
      danger
      valid={typed === name}
      action={action}
      onClose={onClose}
      onSubmit={() =>
        void action
          .run(() =>
            client.call("leaveFederation", { body: { confirm: true } }),
          )
          .then((ok) => {
            if (ok) {
              toast("Left the federation");
              done();
            }
          })
      }
    >
      <Input
        label={`Type ${name} to confirm`}
        mono
        value={typed}
        onChange={val(setTyped)}
      />
    </FormModal>
  );
}
