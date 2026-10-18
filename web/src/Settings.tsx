import { useState } from "react";
import type { Client } from "./api";
import {
  paths,
  useOptional,
  type InstanceSettings,
  type Retention,
  type Storage,
} from "./gaps";
import { formats } from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  PropertyList,
  SegmentedControl,
  Select,
  SettingsLayout,
  SettingsRow,
  SettingsSection,
  Switch,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Needs,
  PageHeading,
  Unavailable,
  bytes,
  chk,
  useAction,
  useResource,
  useToast,
  val,
} from "./ui";

const nav = [
  { id: "general", label: "General", icon: "settings", group: "Instance" },
  { id: "storage", label: "Storage", icon: "hard-drive", group: "Instance" },
  { id: "security", label: "Security", icon: "lock", group: "Instance" },
  {
    id: "retention",
    label: "Retention & cleanup",
    icon: "trash-2",
    group: "Content",
  },
  { id: "about", label: "About", icon: "info", group: "System" },
];

export function SettingsPage({
  client,
  notes,
  setNotes,
}: {
  client: Client;
  notes: boolean;
  setNotes: (v: boolean) => void;
}) {
  return (
    <>
      <PageHeading
        eyebrow="Settings"
        title="Settings"
        text="Instance configuration. Values set by environment are shown read-only."
      />
      <SettingsLayout items={nav} className="settings-wide">
        {(tab: string) =>
          tab === "general" ? (
            <General client={client} />
          ) : tab === "storage" ? (
            <StoragePane client={client} />
          ) : tab === "security" ? (
            <Security client={client} />
          ) : tab === "retention" ? (
            <RetentionPane client={client} />
          ) : (
            <About client={client} notes={notes} setNotes={setNotes} />
          )
        }
      </SettingsLayout>
    </>
  );
}

function useSettings(client: Client) {
  return useOptional<InstanceSettings>(client, paths.settings);
}
function SettingsStatus({
  r,
  what,
}: {
  r:
    | ReturnType<typeof useSettings>
    | ReturnType<typeof useOptional<Retention>>
    | ReturnType<typeof useOptional<Storage>>;
  what: string;
}) {
  if (r.state === "loading") return <Loading what={what} />;
  if (r.state === "unavailable") return <Unavailable what={what} />;
  if (r.state === "error") return <Failure error={r.error} />;
  return null;
}

function General({ client }: { client: Client }) {
  const r = useSettings(client);
  if (r.state !== "ready")
    return (
      <>
        <SettingsStatus r={r} what="Instance settings" />
        <Needs block>instance settings endpoint</Needs>
      </>
    );
  return <GeneralForm client={client} value={r.data} reload={r.reload} />;
}
function GeneralForm({
  client,
  value,
  reload,
}: {
  client: Client;
  value: InstanceSettings;
  reload: () => void;
}) {
  const ro = (k: string) => value.read_only?.includes(k);
  const [name, setName] = useState(value.display_name ?? "");
  const [anon, setAnon] = useState(!!value.default_anonymous_read);
  const action = useAction();
  const toast = useToast();
  return (
    <>
      <form
        onSubmit={async (e) => {
          e.preventDefault();
          const ok = await action.run(() =>
            client.extra("PUT", paths.settings, {
              body: {
                ...value,
                display_name: name,
                default_anonymous_read: anon,
              },
            }),
          );
          if (ok) {
            toast("Settings saved");
            reload();
          }
        }}
      >
        <SettingsSection
          title="Instance"
          form
          footer={
            <Button type="submit" disabled={action.busy}>
              {action.busy ? "Saving…" : "Save"}
            </Button>
          }
        >
          <Input
            label="Display name"
            value={name}
            disabled={ro("display_name")}
            onChange={val(setName)}
          />
          <Input
            label="Public URL"
            mono
            value={value.public_url ?? ""}
            disabled
            readOnly
            hint="Set by the server environment. Read-only here."
          />
          <SettingsRow
            label="Anonymous reads for new feeds"
            description="New feeds start authenticated unless enabled."
          >
            <Switch
              aria-label="Anonymous reads for new feeds"
              checked={anon}
              disabled={ro("default_anonymous_read")}
              onChange={chk(setAnon)}
            />
          </SettingsRow>
          <Failure error={action.error} />
        </SettingsSection>
      </form>
      <SettingsSection title="Immutable versions">
        <SettingsRow
          label="Immutable versions"
          description="Republishing an existing version is rejected."
        >
          <Badge tone="good" icon="lock">
            Always on
          </Badge>
        </SettingsRow>
      </SettingsSection>
    </>
  );
}

function StoragePane({ client }: { client: Client }) {
  const r = useOptional<Storage>(client, paths.storage);
  if (r.state !== "ready")
    return (
      <>
        <SettingsStatus r={r} what="Storage configuration" />
        <Needs block>storage config read</Needs>
      </>
    );
  const s = r.data;
  return (
    <SettingsSection
      title="Blob storage"
      description="Configured by environment. Changes need a restart."
    >
      <PropertyList
        items={[
          { label: "Backend", value: s.backend ?? "—", mono: true },
          { label: "Endpoint", value: s.endpoint ?? "—", mono: true },
          {
            label: "Bucket / prefix",
            value: `${s.bucket ?? "—"} / ${s.prefix ?? ""}`,
            mono: true,
          },
          {
            label: "Path style",
            value:
              s.path_style == null
                ? "—"
                : s.path_style
                  ? "Enabled"
                  : "Disabled",
          },
          {
            label: "Upload staging",
            value: `${s.staging_path ?? "—"} · ${bytes(s.staging_used_bytes)} / ${bytes(s.staging_capacity_bytes)}`,
            mono: true,
          },
        ]}
      />
    </SettingsSection>
  );
}

function Security({ client }: { client: Client }) {
  const r = useSettings(client);
  return (
    <>
      {r.state === "ready" ? (
        <SecurityForm client={client} value={r.data} reload={r.reload} />
      ) : (
        <SettingsSection title="Sessions">
          <SettingsStatus r={r} what="Session settings" />
          <Needs block>instance settings endpoint</Needs>
        </SettingsSection>
      )}
      <SettingsSection title="Encryption keys">
        <SettingsRow
          label="Rotation"
          description="Run on the host. Not available from the console."
        >
          <Mono muted>ant rotate-secrets</Mono>
        </SettingsRow>
      </SettingsSection>
    </>
  );
}
function SecurityForm({
  client,
  value,
  reload,
}: {
  client: Client;
  value: InstanceSettings;
  reload: () => void;
}) {
  const hours = String(value.session_max_age_hours ?? 12);
  const idle = String(value.idle_timeout_minutes ?? 30);
  const [age, setAge] = useState(hours);
  const [timeout, setTimeoutValue] = useState(idle);
  const locked = value.read_only?.includes("session_max_age_hours");
  const action = useAction();
  const toast = useToast();
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        const ok = await action.run(() =>
          client.extra("PUT", paths.settings, {
            body: {
              ...value,
              session_max_age_hours: Number(age),
              idle_timeout_minutes: Number(timeout),
            },
          }),
        );
        if (ok) {
          toast("Settings saved");
          reload();
        }
      }}
    >
      <SettingsSection
        title="Sessions"
        form
        footer={
          <Button type="submit" disabled={action.busy || locked}>
            {action.busy ? "Saving…" : "Save"}
          </Button>
        }
        footerNote={locked ? "Set by environment; read-only." : undefined}
      >
        <SettingsRow
          label="Session length"
          description="Maximum browser session age."
        >
          <SegmentedControl
            aria-label="Session length"
            options={[
              { value: "8", label: "8 h" },
              { value: "12", label: "12 h" },
              { value: "24", label: "24 h" },
            ]}
            value={age}
            onChange={setAge}
          />
        </SettingsRow>
        <SettingsRow
          label="Idle timeout"
          description="Sign out browser sessions after inactivity."
        >
          <SegmentedControl
            mono
            aria-label="Idle timeout"
            options={[
              { value: "15", label: "15m" },
              { value: "30", label: "30m" },
              { value: "60", label: "1h" },
            ]}
            value={timeout}
            onChange={setTimeoutValue}
          />
        </SettingsRow>
        <Failure error={action.error} />
      </SettingsSection>
    </form>
  );
}

function RetentionPane({ client }: { client: Client }) {
  const r = useOptional<Retention>(client, paths.retention);
  return (
    <>
      <Alert tone="info" title="Proposed design">
        Retention and garbage collection are not yet supported by the management
        API. Publication is immutable; unreferenced blobs are collected on the
        host with <Mono>ant gc</Mono>.
      </Alert>
      {r.state === "ready" ? (
        <RetentionForm client={client} value={r.data} reload={r.reload} />
      ) : (
        <>
          <SettingsStatus r={r} what="Retention policies" />
          <Needs block>retention policies + GC jobs</Needs>
        </>
      )}
    </>
  );
}
function RetentionForm({
  client,
  value,
  reload,
}: {
  client: Client;
  value: Retention;
  reload: () => void;
}) {
  const [days, setDays] = useState(
    value.evict_cached_after_days == null
      ? "never"
      : String(value.evict_cached_after_days),
  );
  const [gc, setGc] = useState(!!value.gc_unreferenced_blobs);
  const action = useAction();
  const toast = useToast();
  return (
    <form
      onSubmit={async (e) => {
        e.preventDefault();
        const ok = await action.run(() =>
          client.extra("PUT", paths.retention, {
            body: {
              ...value,
              evict_cached_after_days: days === "never" ? null : Number(days),
              gc_unreferenced_blobs: gc,
            },
          }),
        );
        if (ok) {
          toast("Retention saved");
          reload();
        }
      }}
    >
      <SettingsSection
        title="Cached content"
        form
        footer={
          <Button type="submit" disabled={action.busy}>
            {action.busy ? "Saving…" : "Save"}
          </Button>
        }
      >
        <SettingsRow
          label="Evict cached versions not pulled in"
          description="Remote sources only"
        >
          <Select
            size="sm"
            aria-label="Evict cached versions after"
            options={[
              { value: "30", label: "30 days" },
              { value: "90", label: "90 days" },
              { value: "never", label: "Never" },
            ]}
            value={days}
            onChange={val(setDays)}
          />
        </SettingsRow>
        <SettingsRow
          label="Garbage-collect unreferenced blobs"
          description={value.gc_schedule}
        >
          <Switch
            aria-label="Garbage-collect unreferenced blobs"
            checked={gc}
            onChange={chk(setGc)}
          />
        </SettingsRow>
        <Failure error={action.error} />
      </SettingsSection>
    </form>
  );
}

function About({
  client,
  notes,
  setNotes,
}: {
  client: Client;
  notes: boolean;
  setNotes: (v: boolean) => void;
}) {
  const status = useResource(() => client.call("getStatus"), [client]);
  return (
    <>
      <SettingsSection title="About">
        <Failure error={status.error} />
        {status.loading ? (
          <Loading what="service status" />
        ) : (
          <PropertyList
            items={[
              {
                label: "Service",
                value: status.value?.data.name ?? "—",
                mono: true,
              },
              {
                label: "Management API",
                value: status.value?.data.api_version ?? "—",
                mono: true,
              },
              {
                label: "Configurable formats",
                value: formats.join(", "),
                mono: true,
              },
            ]}
          />
        )}
        <p className="muted small">
          The API does not report which protocols this deployment serves; a
          configured feed does not establish working publish, pull-through or
          replica support.
        </p>
      </SettingsSection>
      <SettingsSection title="Console">
        <SettingsRow
          label="Show backend notes"
          description="Marks console features that depend on management API endpoints not available yet."
        >
          <Switch
            aria-label="Show backend notes"
            checked={notes}
            onChange={chk(setNotes)}
          />
        </SettingsRow>
      </SettingsSection>
    </>
  );
}
