import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  FederationMember,
  FederationQuarantine,
  FederationReplicationRule,
  FederationRuleCompliance,
  FederationRuleStatus,
  FederationStatus,
  FederationTopologyFeedView,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Checkbox,
  Input,
  Modal,
  Select,
  Table,
} from "./ds/bundle";
import {
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
import { RouteModal, RoutingModal, routingSummary } from "./FederationRouting";

const stateTone: Record<string, string> = {
  in_sync: "good",
  ready: "good",
  seeding: "info",
  lagging: "warn",
  pending: "warn",
  failed: "bad",
  conflict: "bad",
  missing_feed: "warn",
  disabled: "neutral",
  unknown: "neutral",
};

function targetsText(
  r: FederationReplicationRule,
  members: FederationMember[],
) {
  if (r.targets.members?.length)
    return r.targets.members
      .map((id) => members.find((m) => m.instance_id === id)?.name ?? id)
      .join(", ");
  return Object.entries(r.targets.labels ?? {})
    .map(([k, v]) => `${k}=${v}`)
    .join(", ");
}

function parseLabels(text: string): Record<string, string> | undefined {
  const out: Record<string, string> = {};
  for (const part of text.split(",")) {
    const t = part.trim();
    if (!t) continue;
    const [k, v] = t.split("=");
    if (!k || v === undefined) return undefined;
    out[k.trim()] = v.trim();
  }
  return Object.keys(out).length ? out : undefined;
}

/** Coordinated replication: feed keys and homes, rules, compliance, transfers. */
export function FederationReplication({
  client,
  st,
  members,
  focusRule,
}: {
  client: Client;
  st: FederationStatus;
  members: FederationMember[];
  /** Rule whose compliance opens first (deep link from a replication row). */
  focusRule?: string;
}) {
  const topology = useOptionalCall(client, "getFederationTopology");
  const local = useOptionalCall(client, "getFederationReplicationStatus");
  const authority = st.role === "authority";
  const [dialog, setDialog] = useState<"feed" | "rule" | undefined>();
  const [transfer, setTransfer] = useState<FederationTopologyFeedView>();
  const [compliance, setCompliance] = useState<string | undefined>(focusRule);
  const [route, setRoute] = useState<string>();
  const [routing, setRouting] = useState<FederationTopologyFeedView>();
  const action = useAction();
  const toast = useToast();
  const view = topology.state === "ready" ? topology.data : undefined;
  const reload = () => {
    topology.reload();
    local.reload();
  };
  const fed = st.federation_name ?? "";
  return (
    <>
      <SectionHead title="Federated feeds" count={view?.feeds.length}>
        {authority && (
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            onClick={() => setDialog("feed")}
          >
            Add feed key
          </Button>
        )}
      </SectionHead>
      {topology.state === "loading" && <Loading what="topology" />}
      {topology.state === "error" && <Failure error={topology.error} />}
      <Failure error={action.error} />
      {view &&
        (view.feeds.length ? (
          <Table
            columns={[
              { key: "key", label: "Feed key", mono: true },
              {
                key: "home",
                label: "Home",
                render: (f: FederationTopologyFeedView) => f.home_name,
              },
              {
                key: "role",
                label: "On this member",
                render: (f: FederationTopologyFeedView) => (
                  <span className="row tight">
                    <Badge tone={f.local_role === "home" ? "info" : undefined}>
                      {f.local_role}
                    </Badge>
                    {f.frozen && <Badge tone="warn">frozen</Badge>}
                  </span>
                ),
              },
              {
                key: "routing",
                label: "Read routing",
                render: (f: FederationTopologyFeedView) => (
                  <span className="row tight">
                    <span>{routingSummary(f.routing)}</span>
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => setRoute(f.key)}
                    >
                      Explain
                    </Button>
                    {authority && (
                      <Button
                        size="sm"
                        variant="secondary"
                        onClick={() => setRouting(f)}
                      >
                        Edit
                      </Button>
                    )}
                  </span>
                ),
              },
              {
                key: "transfer",
                label: "Transfer",
                render: (f: FederationTopologyFeedView) =>
                  f.transfer
                    ? `${f.transfer.mode} · ${f.transfer.state}${f.transfer.reason ? ` (${f.transfer.reason})` : ""}`
                    : "—",
              },
              {
                key: "actions",
                label: "",
                align: "right",
                render: (f: FederationTopologyFeedView) =>
                  authority &&
                  (f.transfer &&
                  (f.transfer.state === "freeze" ||
                    f.transfer.state === "drain") ? (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={action.busy}
                      onClick={async () => {
                        const [fedName, slug] = f.key.split("/");
                        if (
                          await action.run(() =>
                            client.call("abortFederationFeedTransfer", {
                              params: { fed: fedName, slug },
                              body: { confirm: true },
                            }),
                          )
                        ) {
                          toast("Transfer aborted");
                          reload();
                        }
                      }}
                    >
                      Abort transfer
                    </Button>
                  ) : (
                    <Button
                      size="sm"
                      variant="secondary"
                      disabled={
                        !!f.transfer &&
                        f.transfer.state !== "complete" &&
                        f.transfer.state !== "aborted"
                      }
                      onClick={() => setTransfer(f)}
                    >
                      Transfer home
                    </Button>
                  )),
              },
            ]}
            rows={view.feeds}
          />
        ) : (
          <p className="muted">
            No federated feeds. The authority publishes feed keys with their
            single home member.
          </p>
        ))}
      <SectionHead title="Replication rules" count={view?.rules.length}>
        {authority && view && view.feeds.length > 0 && (
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            onClick={() => setDialog("rule")}
          >
            Add rule
          </Button>
        )}
      </SectionHead>
      {view &&
        (view.rules.length ? (
          <Table
            columns={[
              { key: "feed_key", label: "Feed key", mono: true },
              {
                key: "targets",
                label: "Targets",
                render: (r: FederationReplicationRule) =>
                  targetsText(r, members),
              },
              {
                key: "mode",
                label: "Mode",
                render: (r: FederationReplicationRule) =>
                  `${r.mode} · ${r.interval_seconds}s`,
              },
              {
                key: "deletion_policy",
                label: "Deletions",
              },
              {
                key: "revision",
                label: "Rev.",
                align: "right",
              },
              {
                key: "actions",
                label: "",
                align: "right",
                render: (r: FederationReplicationRule) => (
                  <span className="row tight">
                    <Button
                      size="sm"
                      variant="secondary"
                      onClick={() => setCompliance(r.id)}
                    >
                      Compliance
                    </Button>
                    {authority && (
                      <Button
                        size="sm"
                        variant="secondary"
                        disabled={action.busy}
                        onClick={async () => {
                          if (
                            await action.run(() =>
                              client.call("deleteFederationReplicationRule", {
                                params: { id: r.id },
                              }),
                            )
                          ) {
                            toast(
                              "Rule removed",
                              "Targets keep the replicated content; their jobs are disabled and released.",
                            );
                            reload();
                          }
                        }}
                      >
                        Remove
                      </Button>
                    )}
                  </span>
                ),
              },
            ]}
            rows={view.rules}
          />
        ) : (
          <p className="muted">No replication rules.</p>
        ))}
      {local.state === "ready" && (
        <LocalReplication
          client={client}
          items={local.data.items}
          quarantines={local.data.quarantines}
          changed={reload}
        />
      )}
      {dialog === "feed" && (
        <FeedKeyModal
          client={client}
          federation={fed}
          members={members}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {dialog === "rule" && view && (
        <RuleModal
          client={client}
          feeds={view.feeds}
          members={members}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
      {transfer && (
        <TransferModal
          client={client}
          feed={transfer}
          members={members}
          onClose={() => setTransfer(undefined)}
          done={() => {
            setTransfer(undefined);
            reload();
          }}
        />
      )}
      {route && (
        <RouteModal
          client={client}
          feedKey={route}
          onClose={() => setRoute(undefined)}
        />
      )}
      {routing && (
        <RoutingModal
          client={client}
          feed={routing}
          onClose={() => setRouting(undefined)}
          done={() => {
            setRouting(undefined);
            toast(
              "Routing published",
              "Members apply it with the next topology generation.",
            );
            reload();
          }}
        />
      )}
      {compliance && (
        <ComplianceModal
          client={client}
          rule={compliance}
          onClose={() => setCompliance(undefined)}
        />
      )}
    </>
  );
}

function LocalReplication({
  client,
  items,
  quarantines,
  changed,
}: {
  client: Client;
  items: FederationRuleStatus[];
  quarantines: FederationQuarantine[];
  changed: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const open = quarantines.filter((q) => !q.resolved_at);
  return (
    <>
      <SectionHead title="Replication on this member" count={items.length} />
      <Failure error={action.error} />
      {items.length ? (
        <Table
          columns={[
            { key: "feed_key", label: "Feed key", mono: true },
            {
              key: "state",
              label: "State",
              render: (s: FederationRuleStatus) => (
                <span className="row tight">
                  <Badge tone={stateTone[s.state]}>{s.state}</Badge>
                  {s.adopted && <Badge>adopted</Badge>}
                </span>
              ),
            },
            {
              key: "phase",
              label: "Phase",
              render: (s: FederationRuleStatus) => s.phase ?? "—",
            },
            {
              key: "lag",
              label: "Lag",
              align: "right",
              render: (s: FederationRuleStatus) =>
                s.lag_seconds != null ? `${Math.round(s.lag_seconds)}s` : "—",
            },
            {
              key: "success",
              label: "Last success",
              render: (s: FederationRuleStatus) => when(s.last_success_at),
            },
            {
              key: "detail",
              label: "Detail",
              render: (s: FederationRuleStatus) => s.detail ?? "",
            },
          ]}
          rows={items}
        />
      ) : (
        <p className="muted">No replication rule targets this member.</p>
      )}
      {open.length > 0 && (
        <Alert tone="bad" title="Quarantined content after a forced transfer">
          Writes this member accepted as the old home are kept in detached
          sources and never merged. Review them, then mark them resolved.
        </Alert>
      )}
      {quarantines.length > 0 && (
        <Table
          columns={[
            { key: "feed_key", label: "Feed key", mono: true },
            { key: "source_name", label: "Quarantined source", mono: true },
            {
              key: "divergent_changes",
              label: "Divergent changes",
              align: "right",
              render: (q: FederationQuarantine) =>
                q.divergent_changes ?? "unknown",
            },
            {
              key: "created_at",
              label: "Since",
              render: (q: FederationQuarantine) => when(q.created_at),
            },
            {
              key: "actions",
              label: "",
              align: "right",
              render: (q: FederationQuarantine) =>
                q.resolved_at ? (
                  `resolved ${when(q.resolved_at)}`
                ) : (
                  <Button
                    size="sm"
                    variant="secondary"
                    disabled={action.busy}
                    onClick={async () => {
                      if (
                        await action.run(() =>
                          client.call("resolveFederationQuarantine", {
                            params: { id: q.id },
                            body: { confirm: true },
                          }),
                        )
                      ) {
                        toast("Quarantine marked resolved");
                        changed();
                      }
                    }}
                  >
                    Mark resolved
                  </Button>
                ),
            },
          ]}
          rows={quarantines}
        />
      )}
    </>
  );
}

function FeedKeyModal({
  client,
  federation,
  members,
  onClose,
  done,
}: {
  client: Client;
  federation: string;
  members: FederationMember[];
  onClose: () => void;
  done: () => void;
}) {
  const [slug, setSlug] = useState("");
  const [home, setHome] = useState("");
  const [format, setFormat] = useState("");
  const action = useAction();
  const active = members.filter((m) => m.status === "active");
  return (
    <Modal
      title="Add a federated feed key"
      description="Each feed key has exactly one home member. The descriptor seeds destination feeds and never overwrites local settings."
      onClose={onClose}
      width={560}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={
              action.busy || !home || !/^[a-z0-9][a-z0-9_-]*$/.test(slug)
            }
            onClick={async () => {
              if (
                await action.run(() =>
                  client.call("createFederationFeed", {
                    body: {
                      feed_key: `${federation}/${slug}`,
                      home_member_id: home,
                      ...(format
                        ? { descriptor: { format, anonymous_read: false } }
                        : {}),
                    },
                  }),
                )
              )
                done();
            }}
          >
            Add
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
        label="Home member"
        value={home}
        onChange={val(setHome)}
        options={[
          { value: "", label: "Choose a member" },
          ...active.map((m) => ({ value: m.instance_id, label: m.name })),
        ]}
      />
      <Input
        label="Format (empty: taken from the authority's bound feed)"
        mono
        value={format}
        onChange={val(setFormat)}
      />
    </Modal>
  );
}

function RuleModal({
  client,
  feeds,
  members,
  onClose,
  done,
}: {
  client: Client;
  feeds: FederationTopologyFeedView[];
  members: FederationMember[];
  onClose: () => void;
  done: () => void;
}) {
  const [key, setKey] = useState(feeds[0]?.key ?? "");
  const [labels, setLabels] = useState("");
  const [interval, setInterval] = useState("300");
  const [propagate, setPropagate] = useState(false);
  const [cached, setCached] = useState(true);
  const action = useAction();
  const parsed = parseLabels(labels);
  void members;
  return (
    <Modal
      title="Add a replication rule"
      description="Every target member creates or adopts one job that pulls the feed from its home over mTLS. Existing matching jobs are adopted without reseed."
      onClose={onClose}
      width={560}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy || !key || !parsed || !(+interval > 0)}
            onClick={async () => {
              if (
                await action.run(() =>
                  client.call("createFederationReplicationRule", {
                    body: {
                      feed_key: key,
                      targets: { labels: parsed },
                      interval_seconds: +interval,
                      include_cached: cached,
                      deletion_policy: propagate ? "propagate" : "ignore",
                    },
                  }),
                )
              )
                done();
            }}
          >
            Add rule
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      <Select
        label="Feed key"
        value={key}
        onChange={val(setKey)}
        options={feeds.map((f) => ({ value: f.key, label: f.key }))}
      />
      <Input
        label="Target labels (site=kw, …)"
        mono
        value={labels}
        onChange={val(setLabels)}
      />
      <Input
        label="Interval (seconds)"
        value={interval}
        onChange={val(setInterval)}
      />
      <Checkbox
        label="Include the home's pull-through cache"
        checked={cached}
        onChange={chk(setCached)}
      />
      <Checkbox
        label="Propagate deletions"
        description="Deletes only touch content the job replicated."
        checked={propagate}
        onChange={chk(setPropagate)}
      />
    </Modal>
  );
}

function TransferModal({
  client,
  feed,
  members,
  onClose,
  done,
}: {
  client: Client;
  feed: FederationTopologyFeedView;
  members: FederationMember[];
  onClose: () => void;
  done: () => void;
}) {
  const [to, setTo] = useState("");
  const [forced, setForced] = useState(false);
  const [loss, setLoss] = useState(false);
  const [confirm, setConfirm] = useState(false);
  const action = useAction();
  const candidates = members.filter(
    (m) => m.status === "active" && m.instance_id !== feed.home,
  );
  const [fedName, slug] = feed.key.split("/");
  return (
    <Modal
      title={`Transfer the home of ${feed.key}`}
      description={`Current home: ${feed.home_name}. The target must hold a replica (a rule for this key targets it).`}
      onClose={onClose}
      width={600}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="danger"
            disabled={action.busy || !to || !confirm || (forced && !loss)}
            onClick={async () => {
              if (
                await action.run(() =>
                  client.call("startFederationFeedTransfer", {
                    params: { fed: fedName, slug },
                    body: {
                      to_member_id: to,
                      mode: forced ? "forced" : "planned",
                      confirm: true,
                      accept_data_loss: forced && loss,
                    },
                  }),
                )
              )
                done();
            }}
          >
            Start transfer
          </Button>
        </>
      }
    >
      <Failure error={action.error} />
      <Select
        label="New home"
        value={to}
        onChange={val(setTo)}
        options={[
          { value: "", label: "Choose a member" },
          ...candidates.map((m) => ({ value: m.instance_id, label: m.name })),
        ]}
      />
      <Checkbox
        label="Forced transfer (the home is lost)"
        description="Skips freeze and drain. Writes the target never received are quarantined on the old home when it returns."
        checked={forced}
        onChange={chk(setForced)}
      />
      {forced && (
        <Checkbox
          label="I accept possible data loss"
          checked={loss}
          onChange={chk(setLoss)}
        />
      )}
      {!forced && (
        <Alert tone="warn" title="Planned transfer">
          The home rejects publishes (503 feed_transferring) until the target
          has drained to the frozen head and is promoted; it aborts on timeout.
        </Alert>
      )}
      <Checkbox
        label={`Move the home of ${feed.key}`}
        checked={confirm}
        onChange={chk(setConfirm)}
      />
    </Modal>
  );
}

function ComplianceModal({
  client,
  rule,
  onClose,
}: {
  client: Client;
  rule: string;
  onClose: () => void;
}) {
  const c = useOptionalCall(client, "getFederationReplicationRuleCompliance", {
    params: { id: rule },
  });
  const data: FederationRuleCompliance | undefined =
    c.state === "ready" ? c.data : undefined;
  return (
    <Modal
      title="Rule compliance"
      onClose={onClose}
      width={760}
      actions={<Button onClick={onClose}>Done</Button>}
    >
      {c.state === "loading" && <Loading what="compliance" />}
      {c.state === "error" && <Failure error={c.error} />}
      {data && (
        <>
          <p className="muted">
            <Mono>{data.rule.feed_key}</Mono> — revision {data.rule.revision}
          </p>
          <Table
            columns={[
              {
                key: "member_name",
                label: "Member",
                render: (s: FederationRuleStatus) =>
                  s.member_name ?? s.member_id,
              },
              {
                key: "state",
                label: "State",
                render: (s: FederationRuleStatus) => (
                  <Badge tone={stateTone[s.state]}>{s.state}</Badge>
                ),
              },
              {
                key: "lag",
                label: "Lag",
                align: "right",
                render: (s: FederationRuleStatus) =>
                  s.lag_seconds != null ? `${Math.round(s.lag_seconds)}s` : "—",
              },
              {
                key: "detail",
                label: "Detail",
                render: (s: FederationRuleStatus) => s.detail ?? "",
              },
            ]}
            rows={data.members}
          />
        </>
      )}
    </Modal>
  );
}
