import { useState } from "react";
import type { Client } from "./api";
import { useAllFeeds } from "./data";
import { useOptionalCall } from "./gaps";
import type { ReplicationJob, ReplicationPeer } from "./generated";
import { FederationReplication } from "./FederationReplication";
import {
  EditJobModal,
  PeerEditModal,
  TemplateModal,
} from "./ReplicationDialogs";
import {
  buildFlows,
  DirectionCell,
  FeedsCell,
  OutgoingDrawer,
  PeerCell,
  statusFilters,
  statusTone,
  type Direction,
  type Flow,
  type FlowStatus,
} from "./ReplicationFlows";
import {
  Alert,
  Badge,
  Button,
  Drawer,
  DropdownMenu,
  EmptyState,
  IconButton,
  Input,
  Modal,
  PropertyList,
  SegmentedControl,
  Select,
  Switch,
  Table,
  Tabs,
} from "./ds/bundle";
import {
  Copyable,
  Failure,
  Loading,
  PageHeading,
  Unavailable,
  bytes,
  chk,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

export type ReplicationTab = "replication" | "peers" | "federation";

const PULL_MODEL =
  "Incoming jobs run on this instance and pull from a peer; outgoing rows are peers pulling from this instance.";

export function ReplicationPage({
  client,
  initialTab = "replication",
  focusRule,
}: {
  client: Client;
  initialTab?: ReplicationTab;
  /** Federation rule to open in the Federation rules tab. */
  focusRule?: string;
}) {
  const [tab, setTab] = useState<ReplicationTab>(initialTab);
  const [rule, setRule] = useState(focusRule);
  const [direction, setDirection] = useState<"all" | Direction>("all");
  const [statusFilter, setStatusFilter] = useState<"all" | FlowStatus>("all");
  const jobs = useOptionalCall(client, "listReplicationJobs");
  const peers = useOptionalCall(client, "listReplicationPeers");
  const outgoing = useOptionalCall(client, "listReplicationOutgoing");
  const fedStatus = useOptionalCall(client, "getFederationStatus");
  const fed =
    fedStatus.state === "ready" && fedStatus.data.state === "active"
      ? fedStatus.data
      : undefined;
  const memberCall = useOptionalCall(client, "listFederationMembers");
  const members =
    fed && memberCall.state === "ready" ? memberCall.data.items : [];
  const feeds = useAllFeeds(client);
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string>();
  const [editing, setEditing] = useState<string>();
  const [reviewing, setReviewing] = useState<string>();
  const peerList = peers.state === "ready" ? peers.data.items : [];
  const jobList = jobs.state === "ready" ? jobs.data.items : [];
  const outList = outgoing.state === "ready" ? outgoing.data.items : [];
  const current = jobList.find((j) => j.id === open);
  const editJob = jobList.find((j) => j.id === editing);
  const reviewJob = jobList.find((j) => j.id === reviewing);
  const flows = buildFlows({
    jobs: jobList,
    outgoing: outList,
    peers: peerList,
    members,
    feedSlug: (id) =>
      feeds.items.find((f) => f.id === id)?.slug ?? shortFeed(id),
  });
  const shown = flows.filter(
    (f) =>
      (direction === "all" || f.direction === direction) &&
      (statusFilter === "all" || f.status === statusFilter),
  );
  const openFlow = flows.find((f) => f.key === open);
  const reload = () => {
    jobs.reload();
    peers.reload();
    outgoing.reload();
    fedStatus.reload();
  };
  const openRule = (id?: string) => {
    setRule(id);
    setOpen(undefined);
    setTab("federation");
  };
  const activeTab: ReplicationTab =
    tab === "federation" && !fed ? "replication" : tab;
  const incomingCount = flows.filter((f) => f.direction === "in").length;
  const outgoingCount = flows.length - incomingCount;
  return (
    <>
      <PageHeading
        eyebrow="Content origins"
        title="Replication"
        text="Replication is pull based: the instance that owns a job pulls a feed from its peer. Incoming jobs and the peers pulling from this instance are listed together."
      >
        <Button variant="secondary" icon="refresh-cw" onClick={reload}>
          Refresh
        </Button>
        {activeTab !== "federation" && (
          <Button icon="plus" onClick={() => setCreating(true)}>
            {activeTab === "replication" ? "Create job" : "Add peer"}
          </Button>
        )}
      </PageHeading>
      {jobs.state === "unavailable" && peers.state === "unavailable" && !fed ? (
        <Unavailable what="Replication" />
      ) : (
        <>
          <Tabs
            value={activeTab}
            onChange={(v: string) => setTab(v as ReplicationTab)}
            className="spaced-tabs"
            items={[
              { id: "replication", label: "Replication", count: flows.length },
              { id: "peers", label: "Peers", count: peerList.length },
              ...(fed ? [{ id: "federation", label: "Federation rules" }] : []),
            ]}
          />
          {activeTab === "replication" && (
            <>
              <div className="row wrap spaced">
                <SegmentedControl
                  aria-label="Direction"
                  options={[
                    { value: "all", label: `All (${flows.length})` },
                    { value: "in", label: `Incoming (${incomingCount})` },
                    { value: "out", label: `Outgoing (${outgoingCount})` },
                  ]}
                  value={direction}
                  onChange={(v: string) => setDirection(v as "all" | Direction)}
                />
                <Select
                  aria-label="Status"
                  options={statusFilters}
                  value={statusFilter}
                  onChange={val((v) =>
                    setStatusFilter(v as "all" | FlowStatus),
                  )}
                />
              </div>
              {jobs.state === "loading" && <Loading what="replication" />}
              {jobs.state === "error" && <Failure error={jobs.error} />}
              {outgoing.state === "error" && <Failure error={outgoing.error} />}
              {jobs.state === "ready" &&
                (shown.length ? (
                  <Table
                    rowKey="key"
                    onRowClick={(f: Flow) => setOpen(f.job ? f.job.id : f.key)}
                    columns={[
                      {
                        key: "direction",
                        label: "Direction",
                        render: (f: Flow) => <DirectionCell flow={f} />,
                      },
                      {
                        key: "feed",
                        label: "Feed",
                        render: (f: Flow) => <FeedsCell flow={f} />,
                      },
                      {
                        key: "peer",
                        label: "Peer instance",
                        render: (f: Flow) => <PeerCell flow={f} />,
                      },
                      { key: "mode", label: "Mode" },
                      {
                        key: "status",
                        label: "Status",
                        render: (f: Flow) => (
                          <Badge tone={statusTone[f.status]} dot>
                            {f.statusLabel}
                          </Badge>
                        ),
                      },
                      {
                        key: "lag",
                        label: "Lag",
                        align: "right",
                      },
                      {
                        key: "lastSync",
                        label: "Last sync",
                        render: (f: Flow) => when(f.lastSync),
                      },
                      {
                        key: "rule",
                        label: "Managed by",
                        render: (f: Flow) =>
                          f.rule && fed ? (
                            <span onClick={(e) => e.stopPropagation()}>
                              <Button
                                size="sm"
                                variant="secondary"
                                onClick={() => openRule(f.rule?.id)}
                              >
                                {f.rule.label}
                              </Button>
                            </span>
                          ) : f.rule ? (
                            f.rule.label
                          ) : (
                            "manual"
                          ),
                      },
                      {
                        key: "x",
                        label: "",
                        align: "right",
                        render: (f: Flow) =>
                          f.job ? (
                            <JobMenu
                              client={client}
                              job={f.job}
                              changed={reload}
                              edit={() => setEditing(f.job?.id)}
                              review={() => setReviewing(f.job?.id)}
                            />
                          ) : null,
                      },
                    ]}
                    rows={shown}
                  />
                ) : flows.length ? (
                  <EmptyState
                    icon="filter"
                    title="Nothing matches these filters"
                    description="Change the direction or status filter to see more."
                  />
                ) : (
                  <EmptyState
                    icon="copy"
                    title="No replication yet"
                    description={`${PULL_MODEL} Add a peer, then create a job to replicate one of its feeds.`}
                  />
                ))}
            </>
          )}
          {activeTab === "peers" && (
            <>
              {peers.state === "loading" && <Loading what="peers" />}
              {peers.state === "error" && <Failure error={peers.error} />}
              {peers.state === "ready" &&
                (peerList.length ? (
                  <Table
                    columns={[
                      {
                        key: "name",
                        label: "Peer",
                        render: (p: ReplicationPeer) => (
                          <>
                            {p.name}
                            {p.auth_mode === "federation" && (
                              <Badge tone="info">
                                {members.find(
                                  (m) => m.instance_id === p.member_instance_id,
                                )?.name ?? "federation"}
                              </Badge>
                            )}
                          </>
                        ),
                      },
                      { key: "url", label: "URL", mono: true },
                      {
                        key: "ca_ref",
                        label: "Operator CA",
                        mono: true,
                        render: (p: ReplicationPeer) => p.ca_ref ?? "System",
                      },
                      {
                        key: "updated_at",
                        label: "Updated",
                        render: (p: ReplicationPeer) => when(p.updated_at),
                      },
                      {
                        key: "x",
                        label: "",
                        align: "right",
                        render: (p: ReplicationPeer) => (
                          <PeerMenu client={client} peer={p} changed={reload} />
                        ),
                      },
                    ]}
                    rows={peerList}
                  />
                ) : (
                  <EmptyState
                    icon="server"
                    title="No peers"
                    description={`A peer is another instance you can pull from, with its access token. ${PULL_MODEL}`}
                  />
                ))}
            </>
          )}
          {activeTab === "federation" && fed && (
            <FederationReplication
              client={client}
              st={fed}
              members={members}
              focusRule={rule}
            />
          )}
        </>
      )}
      {creating && activeTab === "peers" && (
        <PeerModal
          client={client}
          onClose={() => setCreating(false)}
          done={() => {
            setCreating(false);
            reload();
          }}
        />
      )}
      {creating && activeTab === "replication" && (
        <JobModal
          client={client}
          peers={peerList}
          feeds={feeds.items}
          onClose={() => setCreating(false)}
          done={() => {
            setCreating(false);
            reload();
          }}
        />
      )}
      {current && (
        <JobDrawer
          current={current}
          onClose={() => setOpen(undefined)}
          review={() => setReviewing(current.id)}
          edit={() => setEditing(current.id)}
        />
      )}
      {openFlow?.out && (
        <OutgoingDrawer
          flow={openFlow}
          onClose={() => setOpen(undefined)}
          openRule={openRule}
        />
      )}
      {editJob && (
        <EditJobModal
          client={client}
          job={editJob}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            reload();
          }}
        />
      )}
      {reviewJob && (
        <TemplateModal
          client={client}
          job={reviewJob}
          onClose={() => setReviewing(undefined)}
          done={reload}
        />
      )}
    </>
  );
}

const shortFeed = (id: string) => (id.length > 10 ? `${id.slice(0, 8)}…` : id);

function JobDrawer({
  current,
  onClose,
  review,
  edit,
}: {
  current: ReplicationJob;
  onClose: () => void;
  review: () => void;
  edit: () => void;
}) {
  return (
    <Drawer
      open
      title={current.name}
      description={<Copyable text={current.id} />}
      onClose={() => onClose()}
      width={560}
      footer={
        <>
          <Button variant="secondary" icon="file-text" onClick={() => review()}>
            Configuration template
          </Button>
          <Button variant="secondary" icon="pencil" onClick={() => edit()}>
            Edit job
          </Button>
          <Button onClick={() => onClose()}>Done</Button>
        </>
      }
    >
      {current.status.reseeding && (
        <Alert tone="info" title="Reseeding">
          The peer purged change history this job still needed, so the job is
          re-reading the full manifest. Content already present is not
          transferred again and nothing here is deleted.
        </Alert>
      )}
      <PropertyList
        items={[
          { label: "Phase", value: current.status.phase, mono: true },
          {
            label: "Checkpoint",
            value: `${current.status.checkpoint} of ${current.status.remote_head ?? "?"}`,
            mono: true,
          },
          {
            label: "Lag",
            value:
              current.status.lag_seconds == null
                ? "—"
                : `${current.status.lag_seconds} s`,
          },
          {
            label: "Seeding",
            value:
              current.status.seed_total == null
                ? "—"
                : `${current.status.seed_done} / ${current.status.seed_total}`,
          },
          {
            label: "Last attempt",
            value: when(current.status.last_attempt_at),
          },
          {
            label: "Last success",
            value: when(current.status.last_success_at),
          },
          { label: "Next run", value: when(current.status.next_run_at) },
          {
            label: "Transferred",
            value: `${count(current.status.blobs_transferred)} blobs · ${bytes(current.status.bytes_transferred)} (${count(current.status.blobs_deduplicated)} deduplicated)`,
          },
          {
            label: "Deletions",
            value: `${count(current.status.deletions_applied)} applied · ${count(current.status.deletions_held)} held`,
          },
          { label: "Conflicts", value: count(current.status.conflicts) },
          {
            label: "Failures",
            value: `${count(current.status.consecutive_failures)} in a row · ${count(current.status.failures_total)} total`,
          },
          {
            label: "Reseeds",
            value: `${count(current.status.reseeds)}${
              current.status.source_low_water == null
                ? ""
                : ` · peer history starts after ${count(current.status.source_low_water)}`
            }`,
          },
          {
            label: "Back-fills",
            value: `${count(current.status.backfills_pending)} pending · ${count(current.status.backfills_completed)} done`,
          },
          { label: "Resumed", value: bytes(current.status.resumed_bytes) },
          ...(current.status.last_error
            ? [{ label: "Last error", value: current.status.last_error }]
            : []),
          {
            label: "Replica source",
            value: current.replica_source_id,
            mono: true,
          },
          {
            label: "Remote feed",
            value: current.remote_feed_id,
            mono: true,
          },
          {
            label: "Managed by",
            value:
              current.origin === "federation"
                ? `Federation rule (${current.federation_feed_key ?? ""})${current.adopted_at ? ", adopted" : ""}`
                : "Local",
          },
          { label: "Deletion policy", value: current.deletion_policy },
          {
            label: "Destination",
            value: current.destination_read_only
              ? "Read-only replica"
              : "Publishes allowed",
          },
          {
            label: "Filters",
            value:
              [
                ...(current.filters.namespaces ?? []).map(
                  (v) => `namespace ${v}`,
                ),
                ...(current.filters.packages ?? []).map((v) => `package ${v}`),
                ...(current.filters.version_prefixes ?? []).map(
                  (v) => `version ${v}*`,
                ),
              ].join(", ") || "None (whole feed)",
          },
        ]}
      />
    </Drawer>
  );
}

function JobMenu({
  client,
  job,
  changed,
  edit,
  review,
}: {
  client: Client;
  job: ReplicationJob;
  changed: () => void;
  edit: () => void;
  review: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const run = async (fn: () => Promise<unknown>, done: string) => {
    const ok = await action.run(fn);
    if (ok) {
      toast(done, job.name);
      changed();
    }
  };
  return (
    <span onClick={(e) => e.stopPropagation()}>
      <DropdownMenu
        align="end"
        trigger={
          <IconButton icon="ellipsis" label={`Actions for ${job.name}`} />
        }
        items={[
          {
            label: "Run now",
            icon: "play",
            onSelect: () =>
              run(
                () =>
                  client.call("triggerReplicationJob", {
                    params: { id: job.id },
                  }),
                "Replication run queued",
              ),
          },
          { label: "Edit job", icon: "pencil", onSelect: edit },
          {
            label: "Configuration template",
            icon: "file-text",
            onSelect: review,
          },
          {
            label: job.enabled ? "Pause" : "Resume",
            icon: job.enabled ? "pause" : "play",
            onSelect: () =>
              run(
                () =>
                  client.call("patchReplicationJob", {
                    params: { id: job.id },
                    body: { revision: job.revision, enabled: !job.enabled },
                  }),
                job.enabled ? "Job paused" : "Job resumed",
              ),
          },
          { divider: true },
          {
            label: "Delete job",
            icon: "trash-2",
            danger: true,
            onSelect: () =>
              run(
                () =>
                  client.call("deleteReplicationJob", {
                    params: { id: job.id },
                  }),
                "Job deleted",
              ),
          },
        ]}
      />
      <Failure error={action.error} />
    </span>
  );
}

function PeerMenu({
  client,
  peer,
  changed,
}: {
  client: Client;
  peer: ReplicationPeer;
  changed: () => void;
}) {
  const action = useAction();
  const toast = useToast();
  const [editing, setEditing] = useState<"token" | "connection">();
  return (
    <span onClick={(e) => e.stopPropagation()}>
      {editing && (
        <PeerEditModal
          client={client}
          peer={peer}
          mode={editing}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            changed();
          }}
        />
      )}
      <DropdownMenu
        align="end"
        trigger={
          <IconButton icon="ellipsis" label={`Actions for ${peer.name}`} />
        }
        items={[
          {
            label: "Rotate token",
            icon: "key-round",
            onSelect: () => setEditing("token"),
          },
          {
            label: "Edit connection",
            icon: "pencil",
            onSelect: () => setEditing("connection"),
          },
          { divider: true },
          {
            label: "Delete peer",
            icon: "trash-2",
            danger: true,
            onSelect: async () => {
              const ok = await action.run(() =>
                client.call("deleteReplicationPeer", {
                  params: { id: peer.id },
                }),
              );
              if (ok) {
                toast("Peer deleted", peer.name);
                changed();
              }
            },
          },
        ]}
      />
      <Failure error={action.error} />
    </span>
  );
}

function PeerModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: () => void;
}) {
  const [name, setName] = useState("");
  const [url, setUrl] = useState("");
  const [ca, setCa] = useState("");
  const [token, setToken] = useState("");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Add peer"
      description="The token is stored encrypted and never shown again. It needs replicate permission on the remote feed."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form="add-peer" type="submit" disabled={action.busy}>
            {action.busy ? "Saving…" : "Add peer"}
          </Button>
        </>
      }
    >
      <form
        id="add-peer"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const submitted = token;
          setToken("");
          const ok = await action.run(() =>
            client.call("createReplicationPeer", {
              body: {
                name,
                url,
                token: submitted,
                ...(ca ? { ca_ref: ca } : {}),
              },
            }),
          );
          if (ok) {
            toast("Peer added", name);
            done();
          }
        }}
      >
        <Input
          label="Peer name"
          required
          maxLength={200}
          value={name}
          onChange={val(setName)}
        />
        <Input
          label="Peer HTTPS URL"
          mono
          required
          type="url"
          pattern="https://.*"
          value={url}
          onChange={val(setUrl)}
        />
        <Input
          label="Operator CA ID"
          mono
          value={ca}
          onChange={val(setCa)}
          hint="Empty uses the system trust store."
        />
        <Input
          label="Access token"
          mono
          required
          type="password"
          autoComplete="new-password"
          value={token}
          onChange={val(setToken)}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

function JobModal({
  client,
  peers,
  feeds,
  onClose,
  done,
}: {
  client: Client;
  peers: ReplicationPeer[];
  feeds: { id: string; slug: string; format: string }[];
  onClose: () => void;
  done: () => void;
}) {
  const [name, setName] = useState("");
  const [peer, setPeer] = useState(peers[0]?.id ?? "");
  const [remote, setRemote] = useState("");
  const [dest, setDest] = useState(feeds[0]?.id ?? "");
  const [mode, setMode] = useState<"continuous" | "scheduled" | "manual">(
    "continuous",
  );
  const [interval, setInterval] = useState("300");
  const [cached, setCached] = useState(false);
  const [propagate, setPropagate] = useState(false);
  const [readOnly, setReadOnly] = useState(true);
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Create replication job"
      description="Replicates one remote feed into a local replica source attached to the destination feed."
      onClose={onClose}
      width={600}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            form="add-job"
            type="submit"
            disabled={action.busy || !peer || !dest}
          >
            {action.busy ? "Creating…" : "Create job"}
          </Button>
        </>
      }
    >
      <form
        id="add-job"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("createReplicationJob", {
              body: {
                name,
                peer_id: peer,
                remote_feed_id: remote,
                dest_feed_id: dest,
                mode,
                ...(mode === "manual"
                  ? {}
                  : { interval_seconds: Number(interval) }),
                include_cached: cached,
                deletion_policy: propagate ? "propagate" : "ignore",
                destination_read_only: readOnly,
              },
            }),
          );
          if (ok) {
            toast("Replication job created", name);
            done();
          }
        }}
      >
        <Input
          label="Job name"
          required
          maxLength={200}
          value={name}
          onChange={val(setName)}
        />
        <Select
          label="Peer"
          options={
            peers.length
              ? peers.map((p) => ({ value: p.id, label: p.name }))
              : [{ value: "", label: "Add a peer first" }]
          }
          value={peer}
          onChange={val(setPeer)}
        />
        <Input
          label="Remote feed ID"
          mono
          required
          pattern="[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}"
          value={remote}
          onChange={val(setRemote)}
          hint="The remote instance cannot be browsed from here; copy the feed ID from it."
        />
        <Select
          label="Local destination feed"
          options={feeds.map((f) => ({
            value: f.id,
            label: `${f.slug} (${f.format})`,
          }))}
          value={dest}
          onChange={val(setDest)}
        />
        <div className="cols">
          <Select
            label="Mode"
            options={["continuous", "scheduled", "manual"]}
            value={mode}
            onChange={val((v) => setMode(v as typeof mode))}
          />
          {mode !== "manual" && (
            <Input
              label="Interval (seconds)"
              type="number"
              mono
              min={1}
              value={interval}
              onChange={val(setInterval)}
            />
          )}
        </div>
        <Switch
          label="Include cached content"
          labelPosition="end"
          checked={cached}
          onChange={chk(setCached)}
        />
        <Switch
          label="Propagate deletions"
          labelPosition="end"
          checked={propagate}
          onChange={chk(setPropagate)}
        />
        <Switch
          label="Read-only replica (reject publishes to the destination feed)"
          labelPosition="end"
          checked={readOnly}
          onChange={chk(setReadOnly)}
        />
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}
