import { useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllFeeds, useAllSources } from "./data";
import { useOptionalCall } from "./gaps";
import type {
  IntegrityFinding,
  RetentionPolicy,
  RetentionRun,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Modal,
  PropertyList,
  SegmentedControl,
  Select,
  Switch,
  Table,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Unavailable,
  chk,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

const DAY = 86400;
const days = (s?: number) => (s ? String(Math.round(s / DAY)) : "");

export function RetentionPane({ client }: { client: Client }) {
  const policies = useOptionalCall(client, "listRetentionPolicies", {
    query: { limit: "100" },
  });
  const runs = useOptionalCall(client, "listRetentionRuns", {
    query: { limit: "10" },
  });
  const findings = useOptionalCall(client, "listIntegrityFindings", {
    query: { limit: "20" },
  });
  const feeds = useAllFeeds(client);
  const sources = useAllSources(client);
  const [editing, setEditing] = useState<RetentionPolicy | "new">();
  const [running, setRunning] = useState(false);
  const [report, setReport] = useState<RetentionRun>();
  if (policies.state === "loading") return <Loading what="retention" />;
  if (policies.state === "unavailable")
    return <Unavailable what="Retention and cleanup" />;
  if (policies.state === "error") return <Failure error={policies.error} />;
  const scope = (p: RetentionPolicy) =>
    p.feed_id
      ? `Feed · ${feeds.items.find((f) => f.id === p.feed_id)?.slug ?? p.feed_id}`
      : p.source_id
        ? `Source · ${sources.items.find((s) => s.id === p.source_id)?.name ?? p.source_id}`
        : "Instance default";
  return (
    <>
      <Alert tone="info" title="Retention removes versions">
        Policies decide which versions are eligible; unreferenced blobs are then
        garbage-collected. Start with a dry run to see what would be deleted.
        Tagged versions and releases are protected unless a policy says
        otherwise.
      </Alert>
      <div className="section-head">
        <h2>Policies</h2>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() => setEditing("new")}
        >
          Add policy
        </Button>
      </div>
      {policies.data.items.length ? (
        <Table
          onRowClick={(p: RetentionPolicy) => setEditing(p)}
          columns={[
            { key: "scope", label: "Scope", render: scope },
            {
              key: "keep_last",
              label: "Keep last",
              align: "right",
              render: (p: RetentionPolicy) => p.keep_last ?? "—",
            },
            {
              key: "age",
              label: "Max age",
              align: "right",
              render: (p: RetentionPolicy) =>
                p.max_age_seconds ? `${days(p.max_age_seconds)} d` : "—",
            },
            {
              key: "idle",
              label: "Max idle",
              align: "right",
              render: (p: RetentionPolicy) =>
                p.max_idle_seconds ? `${days(p.max_idle_seconds)} d` : "—",
            },
            { key: "min_keep", label: "Min keep", align: "right" },
            {
              key: "enabled",
              label: "Status",
              render: (p: RetentionPolicy) => (
                <Badge tone={p.enabled ? "good" : "neutral"} dot>
                  {p.enabled ? "Enabled" : "Disabled"}
                </Badge>
              ),
            },
          ]}
          rows={policies.data.items}
        />
      ) : (
        <p className="muted">No retention policies. Nothing is deleted.</p>
      )}
      <div className="section-head">
        <h2>Runs</h2>
        <Button
          size="sm"
          variant="secondary"
          icon="play"
          onClick={() => setRunning(true)}
        >
          Start run
        </Button>
      </div>
      {runs.state === "ready" &&
        (runs.data.items.length ? (
          <Table
            onRowClick={(r: RetentionRun) => setReport(r)}
            columns={[
              {
                key: "started_at",
                label: "Started",
                render: (r: RetentionRun) => when(r.started_at),
              },
              { key: "trigger", label: "Trigger" },
              {
                key: "dry_run",
                label: "Mode",
                render: (r: RetentionRun) => (r.dry_run ? "Dry run" : "Live"),
              },
              {
                key: "state",
                label: "State",
                render: (r: RetentionRun) => (
                  <Badge
                    tone={
                      r.state === "succeeded"
                        ? "good"
                        : r.state === "failed"
                          ? "bad"
                          : "info"
                    }
                    dot
                  >
                    {r.state}
                  </Badge>
                ),
              },
              {
                key: "report",
                label: "Result",
                render: (r: RetentionRun) =>
                  r.report
                    ? `${count(r.report.versions_deleted)} versions · ${count(r.report.blobs_swept)} blobs swept`
                    : "—",
              },
            ]}
            rows={runs.data.items}
          />
        ) : (
          <p className="muted">No runs yet.</p>
        ))}
      {runs.state === "error" && <Failure error={runs.error} />}
      <div className="section-head">
        <h2>Integrity findings</h2>
      </div>
      {findings.state === "ready" &&
        (findings.data.items.length ? (
          <Table
            columns={[
              { key: "kind", label: "Kind" },
              {
                key: "digest",
                label: "Digest",
                render: (f: IntegrityFinding) => <Mono muted>{f.digest}</Mono>,
              },
              {
                key: "detected_at",
                label: "Detected",
                render: (f: IntegrityFinding) => when(f.detected_at),
              },
            ]}
            rows={findings.data.items}
          />
        ) : (
          <p className="muted">No blob integrity problems found.</p>
        ))}
      {findings.state === "error" && <Failure error={findings.error} />}
      {editing && (
        <PolicyModal
          client={client}
          policy={editing === "new" ? undefined : editing}
          feeds={feeds.items}
          sources={sources.items}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            policies.reload();
          }}
        />
      )}
      {running && (
        <RunModal
          client={client}
          onClose={() => setRunning(false)}
          done={(r) => {
            setRunning(false);
            setReport(r);
            runs.reload();
            findings.reload();
          }}
        />
      )}
      {report && (
        <Modal
          title={`${report.dry_run ? "Dry run" : "Run"} · ${report.state}`}
          description={`Started ${when(report.started_at)}`}
          onClose={() => setReport(undefined)}
          actions={<Button onClick={() => setReport(undefined)}>Done</Button>}
        >
          {report.report ? (
            <PropertyList
              items={[
                {
                  label: "Versions deleted",
                  value: count(report.report.versions_deleted),
                },
                {
                  label: "Protected (release / tagged)",
                  value: `${count(report.report.protected_release)} / ${count(report.report.protected_tagged)}`,
                },
                {
                  label: "Blobs marked / swept",
                  value: `${count(report.report.blobs_marked)} / ${count(report.report.blobs_swept)}`,
                },
                {
                  label: "Integrity checked",
                  value: `${count(report.report.integrity_checked)} (${count(report.report.integrity_mismatches)} mismatches, ${count(report.report.integrity_errors)} errors)`,
                },
                ...(report.report.truncated
                  ? [{ label: "Detail", value: "List truncated" }]
                  : []),
              ]}
            />
          ) : (
            <p className="muted">No report recorded yet.</p>
          )}
          {(report.report?.errors ?? []).length > 0 && (
            <Failure error={Error(report.report!.errors!.join("; "))} />
          )}
        </Modal>
      )}
    </>
  );
}

function RunModal({
  client,
  onClose,
  done,
}: {
  client: Client;
  onClose: () => void;
  done: (r: RetentionRun) => void;
}) {
  const [mode, setMode] = useState("dry");
  const action = useAction();
  const toast = useToast();
  return (
    <Modal
      title="Start retention run"
      description="A dry run reports what would be deleted without changing anything."
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant={mode === "dry" ? "primary" : "danger"}
            disabled={action.busy}
            onClick={async () => {
              let run: RetentionRun | undefined;
              const ok = await action.run(async () => {
                run = (
                  await client.call("startRetentionRun", {
                    body: { dry_run: mode === "dry" },
                  })
                ).data;
              });
              if (ok && run) {
                toast(
                  "Retention run started",
                  mode === "dry" ? "Dry run" : "Live",
                );
                done(run);
              }
            }}
          >
            {action.busy
              ? "Starting…"
              : mode === "dry"
                ? "Start dry run"
                : "Delete now"}
          </Button>
        </>
      }
    >
      <SegmentedControl
        block
        aria-label="Run mode"
        options={[
          { value: "dry", label: "Dry run" },
          { value: "live", label: "Live run" },
        ]}
        value={mode}
        onChange={setMode}
      />
      {mode === "live" && (
        <Alert tone="warn" title="Deletion cannot be undone">
          Versions that match an enabled policy are removed.
        </Alert>
      )}
      <Failure error={action.error} />
    </Modal>
  );
}

function PolicyModal({
  client,
  policy,
  feeds,
  sources,
  onClose,
  done,
}: {
  client: Client;
  policy?: RetentionPolicy;
  feeds: { id: string; slug: string }[];
  sources: { id: string; name: string; kind: string }[];
  onClose: () => void;
  done: () => void;
}) {
  const [kind, setKind] = useState(
    policy?.feed_id ? "feed" : policy?.source_id ? "source" : "instance",
  );
  const [feed, setFeed] = useState(policy?.feed_id ?? feeds[0]?.id ?? "");
  const [source, setSource] = useState(
    policy?.source_id ?? sources[0]?.id ?? "",
  );
  const [keep, setKeep] = useState(String(policy?.keep_last ?? ""));
  const [age, setAge] = useState(days(policy?.max_age_seconds));
  const [idle, setIdle] = useState(days(policy?.max_idle_seconds));
  const [min, setMin] = useState(String(policy?.min_keep ?? 1));
  const [tagged, setTagged] = useState(policy?.include_tagged ?? false);
  const [release, setRelease] = useState(policy?.allow_release_delete ?? false);
  const [enabled, setEnabled] = useState(policy?.enabled ?? true);
  const action = useAction();
  const toast = useToast();
  const num = (v: string) => (v === "" ? undefined : Number(v));
  const rules = {
    keep_last: num(keep),
    max_age_seconds: age === "" ? undefined : Number(age) * DAY,
    max_idle_seconds: idle === "" ? undefined : Number(idle) * DAY,
    min_keep: num(min),
    include_tagged: tagged,
    allow_release_delete: release,
    enabled,
  };
  return (
    <Modal
      title={policy ? "Edit retention policy" : "Add retention policy"}
      description="A version is eligible when any set limit applies. Min keep is always preserved."
      onClose={onClose}
      width={600}
      actions={
        <>
          {policy && (
            <Button
              variant="danger"
              icon="trash-2"
              className="push-left"
              disabled={action.busy}
              onClick={async () => {
                const ok = await action.run(() =>
                  client.call("deleteRetentionPolicy", {
                    params: { id: policy.id },
                    etag: requireETag(`"${policy.revision}"`),
                  }),
                );
                if (ok) {
                  toast("Retention policy removed");
                  done();
                }
              }}
            >
              Remove
            </Button>
          )}
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy}
            onClick={async () => {
              const ok = await action.run(async () => {
                if (policy)
                  await client.call("putRetentionPolicy", {
                    params: { id: policy.id },
                    etag: requireETag(`"${policy.revision}"`),
                    body: rules,
                  });
                else
                  await client.call("createRetentionPolicy", {
                    body: {
                      ...rules,
                      ...(kind === "feed" ? { feed_id: feed } : {}),
                      ...(kind === "source" ? { source_id: source } : {}),
                    },
                  });
              });
              if (ok) {
                toast("Retention policy saved");
                done();
              }
            }}
          >
            {action.busy ? "Saving…" : "Save policy"}
          </Button>
        </>
      }
    >
      {!policy && (
        <Select
          label="Scope"
          options={[
            { value: "instance", label: "Instance default" },
            { value: "feed", label: "One feed" },
            { value: "source", label: "One source" },
          ]}
          value={kind}
          onChange={val(setKind)}
        />
      )}
      {!policy && kind === "feed" && (
        <Select
          label="Feed"
          options={feeds.map((f) => ({ value: f.id, label: f.slug }))}
          value={feed}
          onChange={val(setFeed)}
        />
      )}
      {!policy && kind === "source" && (
        <Select
          label="Source"
          options={sources.map((s) => ({
            value: s.id,
            label: `${s.name} (${s.kind})`,
          }))}
          value={source}
          onChange={val(setSource)}
        />
      )}
      <div className="cols">
        <Input
          label="Keep last (versions)"
          type="number"
          mono
          min={1}
          value={keep}
          onChange={val(setKeep)}
        />
        <Input
          label="Max age (days)"
          type="number"
          mono
          min={1}
          value={age}
          onChange={val(setAge)}
        />
        <Input
          label="Max idle (days)"
          type="number"
          mono
          min={1}
          value={idle}
          onChange={val(setIdle)}
        />
        <Input
          label="Min keep"
          type="number"
          mono
          min={0}
          value={min}
          onChange={val(setMin)}
        />
      </div>
      <Switch
        label="Include tagged versions"
        labelPosition="end"
        checked={tagged}
        onChange={chk(setTagged)}
      />
      <Switch
        label="Allow deleting release versions"
        labelPosition="end"
        checked={release}
        onChange={chk(setRelease)}
      />
      <Switch
        label="Enabled"
        labelPosition="end"
        checked={enabled}
        onChange={chk(setEnabled)}
      />
      <Failure error={action.error} />
    </Modal>
  );
}
