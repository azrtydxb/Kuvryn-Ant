import type {
  FederationMember,
  ReplicationJob,
  ReplicationOutgoing,
  ReplicationPeer,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Drawer,
  Icon,
  PropertyList,
  Sparkline,
} from "./ds/bundle";
import { Copyable, bytes, count, shortId, when } from "./ui";

export type Direction = "in" | "out";
export type FlowStatus = "ok" | "syncing" | "error" | "paused" | "stale";

/** One row of the unified replication list: a job pulling in, or a consumer pulling out. */
export type Flow = {
  key: string;
  direction: Direction;
  /** Feed on this instance. */
  localFeed: string;
  /** Feed on the other side as far as this instance knows it. */
  remote: string;
  /** Job name: the local job for incoming, the consumer's job for outgoing. */
  jobName?: string;
  peer: string;
  /** Federation member name when the other side is a federation member. */
  member?: string;
  /** Small second line under the peer name. */
  peerNote?: string;
  /** Full id of the remote feed when only a short id could be shown. */
  remoteId?: string;
  mode: string;
  status: FlowStatus;
  statusLabel: string;
  lag: string;
  lastSync?: string;
  /** Federation rule that manages the row; absent for manual rows. */
  rule?: { id?: string; label: string };
  job?: ReplicationJob;
  out?: ReplicationOutgoing;
};

export const statusTone: Record<FlowStatus, string> = {
  ok: "good",
  syncing: "info",
  error: "bad",
  paused: "neutral",
  stale: "warn",
};

export const statusFilters: { value: "all" | FlowStatus; label: string }[] = [
  { value: "all", label: "All statuses" },
  { value: "ok", label: "Healthy" },
  { value: "syncing", label: "Syncing" },
  { value: "error", label: "Failing" },
  { value: "stale", label: "Stale" },
  { value: "paused", label: "Paused" },
];

function lagText(changes?: number, seconds?: number | null) {
  if (changes == null) return "—";
  if (changes === 0) return "in sync";
  const secs = seconds ? ` · ${Math.round(seconds)} s` : "";
  return `${count(changes)} changes${secs}`;
}

function jobStatus(j: ReplicationJob): { s: FlowStatus; label: string } {
  if (!j.enabled) return { s: "paused", label: "Paused" };
  if (j.status.last_error) return { s: "error", label: "Failing" };
  if (j.status.running) return { s: "syncing", label: "Running" };
  if (j.status.reseeding) return { s: "syncing", label: "Reseeding" };
  if (j.status.phase !== "incremental")
    return { s: "syncing", label: j.status.phase.replace("_", " ") };
  return { s: "ok", label: "In sync" };
}

/** Join incoming jobs and outgoing consumers into one list, newest activity first. */
export function buildFlows(input: {
  jobs: ReplicationJob[];
  outgoing: ReplicationOutgoing[];
  peers: ReplicationPeer[];
  members: FederationMember[];
  feedSlug: (id: string) => string;
}): Flow[] {
  const memberName = (id?: string) =>
    id ? input.members.find((m) => m.instance_id === id)?.name : undefined;
  const incoming = input.jobs.map<Flow>((j) => {
    const peer = input.peers.find((p) => p.id === j.peer_id);
    const st = jobStatus(j);
    return {
      key: `in:${j.id}`,
      direction: "in",
      localFeed: input.feedSlug(j.dest_feed_id),
      remote:
        j.remote_feed_slug ??
        j.federation_feed_key ??
        shortId(j.remote_feed_id, 12),
      remoteId:
        j.remote_feed_slug || j.federation_feed_key
          ? undefined
          : j.remote_feed_id,
      jobName: j.name,
      peer: peer?.name ?? shortId(j.peer_id),
      member: memberName(peer?.member_instance_id),
      mode:
        j.mode === "manual" ? "manual" : `${j.mode} · ${j.interval_seconds}s`,
      status: st.s,
      statusLabel: st.label,
      lag: lagText(j.status.changes_behind, j.status.lag_seconds),
      lastSync: j.status.last_success_at,
      rule:
        j.origin === "federation"
          ? {
              id: j.federation_rule_id,
              label: j.federation_feed_key ?? "federation rule",
            }
          : undefined,
      job: j,
    };
  });
  const outgoing = input.outgoing.map<Flow>((o) => ({
    key: `out:${o.id}`,
    direction: "out",
    localFeed: o.feed_slug,
    remote: o.remote_job ?? "",
    jobName: o.remote_job,
    ...outgoingPeer(o, input.peers),
    mode: "pulled by peer",
    status: o.status === "active" ? "ok" : o.status,
    statusLabel:
      o.status === "active"
        ? "Active"
        : o.status === "stale"
          ? "Stale"
          : "Failing",
    lag: lagText(
      o.last_sequence == null ? undefined : o.lag_changes,
      o.lag_seconds,
    ),
    lastSync: o.last_seen,
    rule: o.rule_id ? { id: o.rule_id, label: "federation rule" } : undefined,
    out: o,
  }));
  return [...incoming, ...outgoing];
}

/** Arrow plus a plain label so the direction never depends on color alone. */
export function DirectionCell({ flow }: { flow: Flow }) {
  const incoming = flow.direction === "in";
  return (
    <span className="flow-dir">
      <Icon
        name={incoming ? "arrow-down-to-line" : "arrow-up-from-line"}
        size={15}
      />
      <span>
        <strong>{incoming ? "Incoming" : "Outgoing"}</strong>
        <small>
          {incoming ? "from " : "to "}
          {flow.peer}
        </small>
      </span>
    </span>
  );
}

/** Both ends of a row: this instance's feed and the other side's feed or job. */
export function FeedsCell({ flow }: { flow: Flow }) {
  const incoming = flow.direction === "in";
  const remote = (
    <span title={flow.remoteId ? `Remote feed id ${flow.remoteId}` : undefined}>
      {flow.remote}
    </span>
  );
  return (
    <span className="flow-feeds">
      <span className="mono">
        {incoming ? (
          <>
            {remote} → {flow.localFeed}
          </>
        ) : flow.remote ? (
          <>
            {flow.localFeed} → {remote}
          </>
        ) : (
          flow.localFeed
        )}
      </span>
      {incoming && flow.jobName && (
        <small>
          Job <span>{flow.jobName}</span>
        </small>
      )}
    </span>
  );
}

export function PeerCell({ flow }: { flow: Flow }) {
  return (
    <>
      {flow.peer}
      {flow.member && flow.direction === "in" && (
        <Badge tone="info">member</Badge>
      )}
      {flow.direction === "out" && flow.peerNote && (
        <small>{flow.peerNote}</small>
      )}
    </>
  );
}

/** Read-only detail for a consumer that pulls from this instance. */
export function OutgoingDrawer({
  flow,
  onClose,
  openRule,
}: {
  flow: Flow;
  onClose: () => void;
  openRule: (id?: string) => void;
}) {
  const o = flow.out;
  if (!o) return null;
  const lagSeries = o.history.map((h) => h.lag_changes);
  return (
    <Drawer
      open
      title={`Outgoing to ${o.consumer_name}`}
      description={<Copyable text={o.id} />}
      onClose={onClose}
      width={560}
      footer={<Button onClick={onClose}>Done</Button>}
    >
      <Alert tone="info" title="Pulled by the consumer">
        The consumer owns this job and pulls from this instance. This view is
        read-only; change or pause it on the consuming instance.
      </Alert>
      {o.status === "error" && (
        <Alert tone="bad" title="Last request failed">
          {o.last_error || "The consumer's last request was refused."}
          {o.last_error_at ? ` (${when(o.last_error_at)})` : ""}
        </Alert>
      )}
      <PropertyList
        items={[
          {
            label: "Direction",
            value: `This instance → ${o.consumer_name}`,
          },
          {
            label: "Consumer",
            value:
              o.consumer_kind === "member"
                ? `Federation member ${o.consumer_name}`
                : `Peer token of principal ${o.consumer_name}`,
          },
          ...(o.member_instance_id
            ? [
                {
                  label: "Instance",
                  value: o.member_instance_id,
                  mono: true,
                },
              ]
            : []),
          ...(o.principal_id
            ? [{ label: "Principal", value: o.principal_id, mono: true }]
            : []),
          { label: "Feed", value: `${o.feed_slug} (${o.feed_format})` },
          {
            label: "Remote job",
            value: o.remote_job || "Not reported by the consumer",
          },
          {
            label: "Managed by",
            value: o.rule_id ? (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => openRule(o.rule_id)}
              >
                Federation rule
              </Button>
            ) : (
              "Manual"
            ),
          },
          { label: "Status", value: flow.statusLabel },
          { label: "Last pull", value: when(o.last_seen) },
          { label: "First seen", value: when(o.first_seen) },
          {
            label: "Served up to",
            value:
              o.last_sequence == null
                ? "No change pages served yet"
                : `${count(o.last_sequence)} of ${count(o.head_sequence)}`,
            mono: true,
          },
          {
            label: "Lag",
            value: lagText(
              o.last_sequence == null ? undefined : o.lag_changes,
              o.lag_seconds,
            ),
          },
          { label: "Requests", value: count(o.pulls) },
          { label: "Bytes served", value: bytes(o.bytes_served) },
        ]}
      />
      <h3 className="h4">Lag history</h3>
      {lagSeries.length > 1 ? (
        <>
          <Sparkline data={lagSeries} height={56} />
          <p className="muted small">
            {lagSeries.length} samples, one per five minutes, newest{" "}
            {when(o.history[o.history.length - 1].at)}. Peak lag{" "}
            {count(Math.max(...lagSeries))} changes.
          </p>
        </>
      ) : (
        <p className="muted small">
          History starts when this instance first served the consumer after the
          upgrade; not enough samples yet.
        </p>
      )}
    </Drawer>
  );
}

/**
 * Who the consumer is, best first: the federation member name; a local
 * replication peer whose name appears in the token's principal (a peer called
 * "kw" for the principal "kw-replicate"); the principal itself as a token.
 */
function outgoingPeer(
  o: ReplicationOutgoing,
  peers: ReplicationPeer[],
): { peer: string; member?: string; peerNote: string } {
  if (o.consumer_kind === "member")
    return {
      peer: o.consumer_name,
      member: o.consumer_name,
      peerNote: "federation member",
    };
  const subject = o.consumer_name.toLowerCase();
  const match = peers
    .filter((p) => p.name.length >= 2 && subject.includes(p.name.toLowerCase()))
    .sort((a, b) => b.name.length - a.name.length)[0];
  return match
    ? { peer: match.name, peerNote: `token ${o.consumer_name}` }
    : { peer: o.consumer_name, peerNote: "token" };
}
