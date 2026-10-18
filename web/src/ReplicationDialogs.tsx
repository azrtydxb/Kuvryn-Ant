import { useEffect, useState } from "react";
import type { Client } from "./api";
import type {
  ReplicationJob,
  ReplicationPeer,
  ReplicationTemplateDiff,
  ReplicationTemplatePreview,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Modal,
  Select,
  Switch,
  Table,
  Textarea,
} from "./ds/bundle";
import { Failure, Loading, chk, count, useAction, useToast, val } from "./ui";

// Filter lists are edited as one value per line (commas also separate).
export const splitList = (text: string) => [
  ...new Set(
    text
      .split(/[\n,]/)
      .map((v) => v.trim())
      .filter(Boolean),
  ),
];
const joinList = (list?: string[]) => (list ?? []).join("\n");

// Rotates the stored credential, or edits the connection (URL and operator CA).
export function PeerEditModal({
  client,
  peer,
  mode,
  onClose,
  done,
}: {
  client: Client;
  peer: ReplicationPeer;
  mode: "token" | "connection";
  onClose: () => void;
  done: () => void;
}) {
  const [url, setUrl] = useState(peer.url);
  const [ca, setCa] = useState(peer.ca_ref ?? "");
  const [token, setToken] = useState("");
  const action = useAction();
  const toast = useToast();
  const rotate = mode === "token";
  const id = `edit-peer-${mode}`;
  return (
    <Modal
      title={
        rotate ? `Rotate token · ${peer.name}` : `Edit peer · ${peer.name}`
      }
      description={
        rotate
          ? "The new token replaces the stored one and is never shown again. It needs replicate permission on the remote feed. Running jobs use it from their next request."
          : "Changes apply from the next request of every job using this peer."
      }
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form={id} type="submit" disabled={action.busy}>
            {action.busy ? "Saving…" : rotate ? "Rotate token" : "Save"}
          </Button>
        </>
      }
    >
      <form
        id={id}
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const submitted = token;
          setToken("");
          const ok = await action.run(() =>
            client.call("patchReplicationPeer", {
              params: { id: peer.id },
              body: rotate
                ? { revision: peer.revision, token: submitted }
                : { revision: peer.revision, url, ca_ref: ca },
            }),
          );
          if (ok) {
            toast(rotate ? "Token rotated" : "Peer updated", peer.name);
            done();
          }
        }}
      >
        {rotate ? (
          <Input
            label="New access token"
            mono
            required
            minLength={16}
            type="password"
            autoComplete="new-password"
            value={token}
            onChange={val(setToken)}
          />
        ) : (
          <>
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
          </>
        )}
        <Failure error={action.error} />
      </form>
    </Modal>
  );
}

// Edits mode, interval, filters, cached content and the deletion policy. The
// job's peer, remote feed and destination feed are fixed at creation.
export function EditJobModal({
  client,
  job,
  onClose,
  done,
}: {
  client: Client;
  job: ReplicationJob;
  onClose: () => void;
  done: () => void;
}) {
  const [mode, setMode] = useState(job.mode);
  const [interval, setInterval] = useState(String(job.interval_seconds));
  const [cached, setCached] = useState(job.include_cached);
  const [propagate, setPropagate] = useState(
    job.deletion_policy === "propagate",
  );
  const [namespaces, setNamespaces] = useState(
    joinList(job.filters.namespaces),
  );
  const [packages, setPackages] = useState(joinList(job.filters.packages));
  const [versions, setVersions] = useState(
    joinList(job.filters.version_prefixes),
  );
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title={`Edit job · ${job.name}`}
      description="Filter and cache changes apply to future changes only; content already replicated is never removed by them. The peer, remote feed and destination feed cannot be changed."
      onClose={onClose}
      width={600}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button form="edit-job" type="submit" disabled={action.busy}>
            {action.busy ? "Saving…" : "Save job"}
          </Button>
        </>
      }
    >
      <form
        id="edit-job"
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.call("patchReplicationJob", {
              params: { id: job.id },
              body: {
                revision: job.revision,
                mode,
                interval_seconds: Number(interval),
                include_cached: cached,
                deletion_policy: propagate ? "propagate" : "ignore",
                filters: {
                  namespaces: splitList(namespaces),
                  packages: splitList(packages),
                  version_prefixes: splitList(versions),
                },
              },
            }),
          );
          if (ok) {
            toast("Job updated", job.name);
            done();
          }
        }}
      >
        <div className="cols">
          <Select
            label="Mode"
            options={["continuous", "scheduled", "manual"]}
            value={mode}
            onChange={val((v) => setMode(v as typeof mode))}
          />
          <Input
            label="Interval (seconds)"
            type="number"
            mono
            required
            min={1}
            max={604800}
            disabled={mode === "manual"}
            value={interval}
            onChange={val(setInterval)}
          />
        </div>
        <Textarea
          label="Namespaces"
          mono
          rows={2}
          value={namespaces}
          onChange={val(setNamespaces)}
          hint="One per line. Empty replicates every namespace."
        />
        <Textarea
          label="Packages"
          mono
          rows={2}
          value={packages}
          onChange={val(setPackages)}
          hint="Exact package names, one per line. Empty replicates every package."
        />
        <Textarea
          label="Version prefixes"
          mono
          rows={2}
          value={versions}
          onChange={val(setVersions)}
          hint="For example 1. to match 1.0 and 1.2.3. Empty replicates every version."
        />
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

const diffTone = (d: ReplicationTemplateDiff) =>
  d.action === "blocked"
    ? "bad"
    : d.action === "change" || d.action === "add"
      ? "info"
      : d.action === "preserved"
        ? "good"
        : "neutral";
const show = (v: unknown) =>
  v === undefined || v === null || v === "" ? "—" : String(v);

// Template review: preview the diff against this destination feed, then apply
// exactly the reviewed digest. Destination-specific settings are listed as
// preserved and are never written.
export function TemplateModal({
  client,
  job,
  onClose,
  done,
}: {
  client: Client;
  job: ReplicationJob;
  onClose: () => void;
  done: () => void;
}) {
  const [preview, setPreview] = useState<ReplicationTemplatePreview>();
  const [applied, setApplied] = useState<number>();
  const [showAll, setShowAll] = useState(false);
  const load = useAction();
  const apply = useAction();
  const toast = useToast();
  const { run } = load;
  useEffect(() => {
    let live = true;
    void run(async () => {
      const r = await client.call("previewReplicationTemplate", {
        params: { id: job.id },
        body: {},
      });
      if (live) setPreview(r.data);
    });
    return () => {
      live = false;
    };
    // The preview is loaded once per opened dialog.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, job.id]);
  const rows = (preview?.diffs ?? []).filter(
    (d) => showAll || ["add", "change", "blocked", "info"].includes(d.action),
  );
  return (
    <Modal
      title={`Configuration template · ${job.name}`}
      description="Non-secret feed settings of the remote feed, compared with this destination feed. Content sync is separate. The endpoint, credentials, identity settings and access policy of this instance are never overwritten."
      onClose={onClose}
      width={760}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            {applied == null ? "Cancel" : "Close"}
          </Button>
          {applied == null && (
            <Button
              disabled={
                !preview ||
                apply.busy ||
                preview.blocked > 0 ||
                preview.applicable === 0
              }
              onClick={async () => {
                if (!preview) return;
                const ok = await apply.run(async () => {
                  const r = await client.call("applyReplicationTemplate", {
                    params: { id: job.id },
                    body: { digest: preview.digest },
                  });
                  setApplied(r.data.applied);
                });
                if (ok) {
                  toast("Template applied", job.name);
                  done();
                }
              }}
            >
              {apply.busy ? "Applying…" : "Apply reviewed template"}
            </Button>
          )}
        </>
      }
    >
      <div className="stack">
        {load.busy && !preview && <Loading what="template preview" />}
        <Failure error={load.error} />
        <Failure error={apply.error} />
        {preview && applied != null && (
          <Alert tone="success" title="Template applied">
            {count(applied)} settings changed.
          </Alert>
        )}
        {preview && applied == null && preview.blocked > 0 && (
          <Alert tone="danger" title="This template cannot be applied">
            {count(preview.blocked)} entries are blocked. Resolve them (see the
            notes) or fix the template on the source, then review again.
          </Alert>
        )}
        {preview &&
          applied == null &&
          preview.blocked === 0 &&
          preview.applicable === 0 && (
            <Alert tone="info" title="Nothing to apply">
              This destination already matches the template.
            </Alert>
          )}
        {preview && (
          <>
            <Table
              columns={[
                {
                  key: "setting",
                  label: "Setting",
                  render: (d: ReplicationTemplateDiff) => (
                    <>
                      {d.field}
                      <small>{d.section}</small>
                    </>
                  ),
                },
                {
                  key: "action",
                  label: "Action",
                  render: (d: ReplicationTemplateDiff) => (
                    <Badge tone={diffTone(d)} dot>
                      {d.action}
                    </Badge>
                  ),
                },
                {
                  key: "current",
                  label: "Now",
                  mono: true,
                  render: (d: ReplicationTemplateDiff) => show(d.current),
                },
                {
                  key: "proposed",
                  label: "Template",
                  mono: true,
                  render: (d: ReplicationTemplateDiff) => show(d.proposed),
                },
                {
                  key: "reason",
                  label: "Note",
                  render: (d: ReplicationTemplateDiff) => d.reason ?? "",
                },
              ]}
              rows={rows}
            />
            <Switch
              label="Show unchanged and preserved settings"
              labelPosition="end"
              checked={showAll}
              onChange={chk(setShowAll)}
            />
          </>
        )}
      </div>
    </Modal>
  );
}
