import { useCallback, useEffect, useState } from "react";
import type { Client } from "./api";
import { isUnavailable } from "./gaps";
import type { StorageJob, StorageMigrationStatus } from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Meter,
  Modal,
  PropertyList,
  SettingsSection,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Unavailable,
  bytes,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

const STALE_MS = 90_000;

type Loaded =
  | { state: "loading" }
  | { state: "unavailable" }
  | { state: "error"; error: unknown }
  | { state: "ready"; data: StorageMigrationStatus };

// Polls while a job is active, without flashing the loading state.
function useMigrationStatus(client: Client) {
  const [status, setStatus] = useState<Loaded>({ state: "loading" });
  const [tick, setTick] = useState(0);
  const reload = useCallback(() => setTick((t) => t + 1), []);
  const running =
    status.state === "ready" && status.data.current_job?.state === "running";
  useEffect(() => {
    let live = true;
    client
      .call("getStorageMigration")
      .then((r) => live && setStatus({ state: "ready", data: r.data }))
      .catch((e: unknown) => {
        if (!live) return;
        setStatus(
          isUnavailable(e)
            ? { state: "unavailable" }
            : { state: "error", error: e },
        );
      });
    return () => {
      live = false;
    };
  }, [client, tick]);
  useEffect(() => {
    if (!running) return;
    const t = setInterval(reload, 3000);
    return () => clearInterval(t);
  }, [running, reload]);
  return { status, reload };
}

const stateTone = (j: StorageJob) =>
  j.state === "completed"
    ? "good"
    : j.state === "failed"
      ? "bad"
      : j.state === "paused"
        ? "warn"
        : "info";

const abandoned = (j: StorageJob) =>
  j.state === "running" && Date.now() - Date.parse(j.updated_at) > STALE_MS;

function jobTitle(j: StorageJob) {
  const what =
    j.kind === "migrate"
      ? `Migration ${j.from} → ${j.to}`
      : `Cleanup of ${j.from}`;
  return j.dry_run ? `${what} (dry run)` : what;
}

export function StorageMigrationSection({ client }: { client: Client }) {
  const { status, reload } = useMigrationStatus(client);
  const [dialog, setDialog] = useState<"migrate" | "cleanup">();
  const action = useAction();
  const toast = useToast();
  if (status.state === "loading") return <Loading what="storage migration" />;
  if (status.state === "unavailable")
    return <Unavailable what="Storage migration" />;
  if (status.state === "error") return <Failure error={status.error} />;
  const s = status.data;
  const job = s.current_job;
  const other = s.available_backends.find((b) => b !== s.active_backend);
  const active = job && (job.state === "running" || job.state === "paused");
  async function jobAction(id: string, verb: "pause" | "resume") {
    const ok = await action.run(() =>
      client.call("storageJobAction", {
        params: { id, action: verb },
      }),
    );
    if (ok) {
      toast(verb === "pause" ? "Job paused" : "Job resumed");
      reload();
    }
  }
  return (
    <>
      <SettingsSection
        title="Storage migration"
        description="Move all blobs between disk and S3 while serving. Repository URLs do not change, reads keep working throughout, and the old store stays intact until you clean it up."
      >
        <PropertyList
          items={[
            { label: "Active backend", value: s.active_backend, mono: true },
            {
              label: "Previous backend",
              value: s.previous_backend ? (
                <span className="row tight">
                  <Mono>{s.previous_backend}</Mono>
                  <Badge tone="warn">holds a full copy</Badge>
                </span>
              ) : (
                "—"
              ),
            },
            {
              label: "Available",
              value: s.available_backends.join(", "),
              mono: true,
            },
          ]}
        />
        {!other && (
          <p className="muted small">
            Only one backend is configured. Set the S3 environment variables
            (ANT_S3_*) to enable migration to S3.
          </p>
        )}
        <div className="row wrap">
          {other && !s.cleanup_pending && (
            <Button
              variant="secondary"
              disabled={!!active}
              onClick={() => setDialog("migrate")}
            >
              {`Migrate to ${other}…`}
            </Button>
          )}
          {s.cleanup_pending && (
            <Button
              variant="secondary"
              disabled={!!active}
              onClick={() => setDialog("cleanup")}
            >
              {`Clean up ${s.previous_backend}…`}
            </Button>
          )}
        </div>
        <Failure error={action.error} />
      </SettingsSection>
      {job && (
        <SettingsSection
          title={jobTitle(job)}
          description={`Started ${when(job.started_at)}`}
        >
          <div className="row tight">
            <Badge tone={stateTone(job)} dot>
              {abandoned(job) ? "abandoned" : job.state}
            </Badge>
            {job.state === "completed" && job.switched_at && (
              <span className="muted small">
                Switched {when(job.switched_at)}
              </span>
            )}
          </div>
          <Meter
            label={job.dry_run ? "Checked" : "Progress"}
            valueLabel={`${count(job.processed_blobs)} / ${count(job.total_blobs)} blobs · ${bytes(job.processed_bytes)} / ${bytes(job.total_bytes)}`}
            value={
              job.state === "completed"
                ? 100
                : job.total_blobs
                  ? Math.min(
                      100,
                      Math.round((job.processed_blobs / job.total_blobs) * 100),
                    )
                  : 0
            }
          />
          <PropertyList
            items={[
              {
                label: job.dry_run ? "Would copy" : "Copied",
                value: `${count(job.copied_blobs)} blobs · ${bytes(job.copied_bytes)}`,
              },
              ...(job.kind === "migrate"
                ? [
                    {
                      label: "Already present and verified",
                      value: count(job.skipped_blobs),
                    },
                  ]
                : [
                    {
                      label: job.dry_run ? "Would delete" : "Deleted",
                      value: `${count(job.deleted_blobs)} blobs · ${bytes(job.deleted_bytes)}`,
                    },
                  ]),
              { label: "Failed", value: count(job.failed_blobs) },
              { label: "Pass", value: String(job.pass) },
              ...(job.throttle_bytes_per_second
                ? [
                    {
                      label: "Throttle",
                      value: `${bytes(job.throttle_bytes_per_second)}/s`,
                    },
                  ]
                : []),
            ]}
          />
          {job.last_error && (
            <Alert tone="bad" title="Job failed">
              {job.last_error}. Fix the cause and start the job again; verified
              blobs are skipped.
            </Alert>
          )}
          <div className="row wrap">
            {job.state === "running" && !abandoned(job) && (
              <Button
                variant="secondary"
                disabled={action.busy}
                onClick={() => void jobAction(job.id, "pause")}
              >
                Pause
              </Button>
            )}
            {(job.state === "paused" || abandoned(job)) && (
              <Button
                disabled={action.busy}
                onClick={() => void jobAction(job.id, "resume")}
              >
                Resume
              </Button>
            )}
          </div>
        </SettingsSection>
      )}
      {dialog && (
        <StartModal
          client={client}
          kind={dialog}
          from={s.active_backend}
          to={other}
          previous={s.previous_backend}
          onClose={() => setDialog(undefined)}
          done={() => {
            setDialog(undefined);
            reload();
          }}
        />
      )}
    </>
  );
}

function StartModal({
  client,
  kind,
  from,
  to,
  previous,
  onClose,
  done,
}: {
  client: Client;
  kind: "migrate" | "cleanup";
  from: string;
  to?: string;
  previous?: string;
  onClose: () => void;
  done: () => void;
}) {
  const [dry, setDry] = useState(true);
  const [mb, setMb] = useState("");
  const action = useAction();
  const toast = useToast();
  const mbOK = mb === "" || (Number(mb) >= 0 && Number.isFinite(Number(mb)));
  const target = to === "s3" || to === "disk" ? to : undefined;
  async function start() {
    const ok = await action.run(() =>
      client.call("startStorageJob", {
        body: {
          action: kind,
          dry_run: dry,
          ...(kind === "migrate" && target ? { to: target } : {}),
          ...(mb !== "" && Number(mb) > 0
            ? { throttle_bytes_per_second: Math.round(Number(mb) * 1048576) }
            : {}),
        },
      }),
    );
    if (ok) {
      toast(dry ? "Dry run started" : "Job started");
      done();
    }
  }
  return (
    <Modal
      title={
        kind === "migrate"
          ? `Migrate storage to ${to}`
          : `Clean up ${previous} storage`
      }
      description={
        kind === "migrate"
          ? `Copies and verifies every blob from ${from} to ${to}, then switches. The ${from} store is kept.`
          : `Verifies each blob on ${from} and then deletes its copy from ${previous}.`
      }
      onClose={onClose}
      actions={
        <>
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant={dry || kind === "migrate" ? "primary" : "danger"}
            disabled={action.busy || !mbOK}
            onClick={() => void start()}
          >
            {action.busy
              ? "Starting…"
              : dry
                ? "Start dry run"
                : kind === "migrate"
                  ? "Start migration"
                  : "Delete old copies"}
          </Button>
        </>
      }
    >
      <label className="row tight">
        <input
          type="checkbox"
          checked={dry}
          onChange={(e) => setDry(e.target.checked)}
        />
        <span>Dry run (copy and delete nothing, report what would happen)</span>
      </label>
      {kind === "migrate" && (
        <Input
          label="Throttle (MiB/s, optional)"
          type="number"
          mono
          min={0}
          value={mb}
          onChange={val(setMb)}
          error={mbOK ? undefined : "A non-negative number."}
          hint="Limits read and write throughput so serving traffic is not starved."
        />
      )}
      {!dry && kind === "cleanup" && (
        <Alert tone="warn" title="Deletion cannot be undone">
          After cleanup the {previous} copy is gone and there is no fallback.
        </Alert>
      )}
      <Failure error={action.error} />
    </Modal>
  );
}
