import { useState } from "react";
import type { Client } from "./api";
import { useAllFeeds } from "./data";
import { useOptionalCall } from "./gaps";
import type { ReplicationJob, ReplicationPeer } from "./generated";
import {
  Badge,
  Button,
  Drawer,
  DropdownMenu,
  EmptyState,
  IconButton,
  Input,
  Modal,
  PropertyList,
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

const phaseTone = (j: ReplicationJob) =>
  j.status.last_error ? "bad" : j.status.running ? "info" : "good";

export function ReplicationPage({ client }: { client: Client }) {
  const [tab, setTab] = useState("jobs");
  const jobs = useOptionalCall(client, "listReplicationJobs");
  const peers = useOptionalCall(client, "listReplicationPeers");
  const feeds = useAllFeeds(client);
  const [creating, setCreating] = useState(false);
  const [open, setOpen] = useState<string>();
  const peerList = peers.state === "ready" ? peers.data.items : [];
  const jobList = jobs.state === "ready" ? jobs.data.items : [];
  const current = jobList.find((j) => j.id === open);
  const reload = () => {
    jobs.reload();
    peers.reload();
  };
  return (
    <>
      <PageHeading
        eyebrow="Content origins"
        title="Replication"
        text="Pull feeds from another Kuvryn Ant instance into a local replica source."
      >
        <Button variant="secondary" icon="refresh-cw" onClick={reload}>
          Refresh
        </Button>
        <Button icon="plus" onClick={() => setCreating(true)}>
          {tab === "jobs" ? "Create job" : "Add peer"}
        </Button>
      </PageHeading>
      {jobs.state === "unavailable" && peers.state === "unavailable" ? (
        <Unavailable what="Replication" />
      ) : (
        <>
          <Tabs
            value={tab}
            onChange={setTab}
            className="spaced-tabs"
            items={[
              { id: "jobs", label: "Jobs", count: jobList.length },
              { id: "peers", label: "Peers", count: peerList.length },
            ]}
          />
          {tab === "jobs" && (
            <>
              {jobs.state === "loading" && <Loading what="replication jobs" />}
              {jobs.state === "error" && <Failure error={jobs.error} />}
              {jobs.state === "ready" &&
                (jobList.length ? (
                  <Table
                    onRowClick={(j: ReplicationJob) => setOpen(j.id)}
                    columns={[
                      {
                        key: "name",
                        label: "Job",
                        render: (j: ReplicationJob) => (
                          <>
                            {j.name}
                            <small>
                              {peerList.find((p) => p.id === j.peer_id)?.name ??
                                j.peer_id}
                            </small>
                          </>
                        ),
                      },
                      {
                        key: "dest",
                        label: "Local feed",
                        mono: true,
                        render: (j: ReplicationJob) =>
                          feeds.items.find((f) => f.id === j.dest_feed_id)
                            ?.slug ?? j.dest_feed_id,
                      },
                      { key: "mode", label: "Mode" },
                      {
                        key: "phase",
                        label: "State",
                        render: (j: ReplicationJob) => (
                          <Badge tone={phaseTone(j)} dot>
                            {j.status.running
                              ? "Running"
                              : j.status.last_error
                                ? "Failing"
                                : j.status.phase.replace("_", " ")}
                          </Badge>
                        ),
                      },
                      {
                        key: "lag",
                        label: "Behind",
                        align: "right",
                        render: (j: ReplicationJob) =>
                          j.status.changes_behind == null
                            ? "—"
                            : `${count(j.status.changes_behind)} changes`,
                      },
                      {
                        key: "last",
                        label: "Last success",
                        render: (j: ReplicationJob) =>
                          when(j.status.last_success_at),
                      },
                      {
                        key: "enabled",
                        label: "Status",
                        render: (j: ReplicationJob) => (
                          <Badge tone={j.enabled ? "good" : "neutral"} dot>
                            {j.enabled ? "Enabled" : "Paused"}
                          </Badge>
                        ),
                      },
                      {
                        key: "x",
                        label: "",
                        align: "right",
                        render: (j: ReplicationJob) => (
                          <JobMenu client={client} job={j} changed={reload} />
                        ),
                      },
                    ]}
                    rows={jobList}
                  />
                ) : (
                  <EmptyState
                    icon="copy"
                    title="No replication jobs"
                    description="Add a peer, then create a job to replicate one of its feeds."
                  />
                ))}
            </>
          )}
          {tab === "peers" && (
            <>
              {peers.state === "loading" && <Loading what="peers" />}
              {peers.state === "error" && <Failure error={peers.error} />}
              {peers.state === "ready" &&
                (peerList.length ? (
                  <Table
                    columns={[
                      { key: "name", label: "Peer" },
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
                    description="A peer is another instance you can pull from, with its access token."
                  />
                ))}
            </>
          )}
        </>
      )}
      {creating && tab === "peers" && (
        <PeerModal
          client={client}
          onClose={() => setCreating(false)}
          done={() => {
            setCreating(false);
            reload();
          }}
        />
      )}
      {creating && tab === "jobs" && (
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
        <Drawer
          open
          title={current.name}
          description={<Copyable text={current.id} />}
          onClose={() => setOpen(undefined)}
          width={560}
          footer={<Button onClick={() => setOpen(undefined)}>Done</Button>}
        >
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
                label: "Consecutive failures",
                value: count(current.status.consecutive_failures),
              },
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
              { label: "Deletion policy", value: current.deletion_policy },
            ]}
          />
        </Drawer>
      )}
    </>
  );
}

function JobMenu({
  client,
  job,
  changed,
}: {
  client: Client;
  job: ReplicationJob;
  changed: () => void;
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
  return (
    <span onClick={(e) => e.stopPropagation()}>
      <DropdownMenu
        align="end"
        trigger={
          <IconButton icon="ellipsis" label={`Actions for ${peer.name}`} />
        }
        items={[
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
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}
