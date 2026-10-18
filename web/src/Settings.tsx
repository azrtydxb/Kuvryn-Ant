import { useState } from "react";
import { requireETag, type Client } from "./api";
import { useOptionalCall } from "./gaps";
import { formats, type InstanceSettings } from "./generated";
import {
  Badge,
  Button,
  Input,
  PropertyList,
  SettingsLayout,
  SettingsRow,
  SettingsSection,
  Switch,
  Table,
} from "./ds/bundle";
import { LimitsPane } from "./Limits";
import { RetentionPane } from "./Retention";
import { StorageMigrationSection } from "./StorageMigration";
import { TemplatesPane } from "./Templates";
import {
  Failure,
  Loading,
  Mono,
  PageHeading,
  Unavailable,
  bytes,
  chk,
  count,
  useAction,
  useResource,
  useToast,
  val,
  when,
} from "./ui";

const nav = [
  { id: "general", label: "General", icon: "settings", group: "Instance" },
  { id: "storage", label: "Storage", icon: "hard-drive", group: "Instance" },
  { id: "security", label: "Security", icon: "lock", group: "Instance" },
  { id: "limits", label: "Limits & quotas", icon: "gauge", group: "Instance" },
  {
    id: "templates",
    label: "Policy templates",
    icon: "layers",
    group: "Content",
  },
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
          tab === "general" || tab === "security" ? (
            <InstanceSettingsPane client={client} section={tab} />
          ) : tab === "storage" ? (
            <StoragePane client={client} />
          ) : tab === "limits" ? (
            <LimitsPane client={client} />
          ) : tab === "templates" ? (
            <TemplatesPane client={client} />
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

function InstanceSettingsPane({
  client,
  section,
}: {
  client: Client;
  section: "general" | "security";
}) {
  const r = useOptionalCall(client, "getSettings");
  if (r.state === "loading") return <Loading what="instance settings" />;
  if (r.state === "unavailable")
    return <Unavailable what="Instance settings" />;
  if (r.state === "error") return <Failure error={r.error} />;
  return (
    <SettingsForm
      key={r.data.revision}
      client={client}
      value={r.data}
      section={section}
      reload={r.reload}
    />
  );
}

function SettingsForm({
  client,
  value,
  section,
  reload,
}: {
  client: Client;
  value: InstanceSettings;
  section: "general" | "security";
  reload: () => void;
}) {
  const item = (key: string) => value.items.find((i) => i.key === key);
  const locked = (key: string) => !!item(key)?.read_only;
  const [age, setAge] = useState(
    String(
      Math.round(Number(item("session_max_age_seconds")?.value ?? 0) / 60),
    ),
  );
  const [idle, setIdle] = useState(
    String(
      Math.round(Number(item("session_idle_timeout_seconds")?.value ?? 0) / 60),
    ),
  );
  const [anon, setAnon] = useState(
    item("default_anonymous_read")?.value === "true",
  );
  const action = useAction();
  const toast = useToast();
  const ageOK = Number(age) >= 1 && Number(age) <= 480;
  const idleOK = Number(idle) >= 1 && Number(idle) <= 30;
  async function save() {
    const ok = await action.run(() =>
      client.call("putSettings", {
        etag: requireETag(`"${value.revision}"`),
        body: {
          session_max_age_seconds: Math.round(Number(age) * 60),
          session_idle_timeout_seconds: Math.round(Number(idle) * 60),
          default_anonymous_read: anon,
        },
      }),
    );
    if (ok) {
      toast("Settings saved");
      reload();
    }
  }
  const footer = (
    <Button
      type="submit"
      disabled={action.busy || (section === "security" && !(ageOK && idleOK))}
    >
      {action.busy ? "Saving…" : "Save"}
    </Button>
  );
  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void save();
      }}
    >
      {section === "general" ? (
        <>
          <SettingsSection
            title="Defaults for new feeds"
            form
            footer={footer}
            footerNote={
              value.updated_at ? `Updated ${when(value.updated_at)}` : undefined
            }
          >
            <SettingsRow
              label="Anonymous reads"
              description={
                item("default_anonymous_read")?.description ??
                "Suggested default for new feeds."
              }
            >
              <Switch
                aria-label="Anonymous reads for new feeds"
                checked={anon}
                disabled={locked("default_anonymous_read")}
                onChange={chk(setAnon)}
              />
            </SettingsRow>
            <Failure error={action.error} />
          </SettingsSection>
          <SettingsSection
            title="All settings"
            description="Where each value comes from. Environment-backed values cannot be changed here."
          >
            <PropertyList
              items={value.items.map((i) => ({
                label: i.key,
                mono: true,
                value: (
                  <span className="row tight">
                    {i.value}
                    <Badge tone={i.source === "env" ? "warn" : "neutral"}>
                      {i.source}
                    </Badge>
                    {i.read_only && <Badge tone="outline">read-only</Badge>}
                  </span>
                ),
              }))}
            />
          </SettingsSection>
        </>
      ) : (
        <SettingsSection title="Sessions" form footer={footer}>
          <Input
            label="Session length (minutes)"
            type="number"
            mono
            min={1}
            max={480}
            value={age}
            disabled={locked("session_max_age_seconds")}
            onChange={val(setAge)}
            error={ageOK ? undefined : "Between 1 and 480 minutes."}
            hint="Maximum browser session age. Enforced server-side."
          />
          <Input
            label="Idle timeout (minutes)"
            type="number"
            mono
            min={1}
            max={30}
            value={idle}
            disabled={locked("session_idle_timeout_seconds")}
            onChange={val(setIdle)}
            error={idleOK ? undefined : "Between 1 and 30 minutes."}
            hint="Sign out browser sessions after inactivity."
          />
          <Failure error={action.error} />
          <SettingsRow
            label="Encryption key rotation"
            description="Run on the host. Not available from the console."
          >
            <Mono muted>ant rotate-secrets</Mono>
          </SettingsRow>
        </SettingsSection>
      )}
    </form>
  );
}

function StoragePane({ client }: { client: Client }) {
  const r = useOptionalCall(client, "getStorageUsage");
  if (r.state === "loading") return <Loading what="storage usage" />;
  if (r.state === "unavailable") return <Unavailable what="Storage usage" />;
  if (r.state === "error") return <Failure error={r.error} />;
  const s = r.data;
  return (
    <>
      <SettingsSection
        title="Blob storage"
        description="Configured by environment. Changes need a restart."
      >
        <PropertyList
          items={[
            { label: "Backend", value: s.backend, mono: true },
            { label: "Blobs", value: count(s.blob_count) },
            { label: "Stored", value: bytes(s.blob_bytes) },
            { label: "Logical size", value: bytes(s.logical_bytes) },
            { label: "Deduplication saved", value: bytes(s.dedup_saved_bytes) },
            {
              label: "Unreferenced",
              value: `${bytes(s.unreferenced_bytes)} · ${count(s.gc_candidates)} collectable`,
            },
            ...(s.staging_bytes != null
              ? [{ label: "Upload staging", value: bytes(s.staging_bytes) }]
              : []),
            { label: "Measured", value: when(s.measured_at) },
          ]}
        />
      </SettingsSection>
      <StorageMigrationSection client={client} />
      {s.domains.length > 0 && (
        <Table
          rowKey="domain_id"
          columns={[
            { key: "name", label: "Storage domain", mono: true },
            {
              key: "blob_count",
              label: "Blobs",
              align: "right",
              render: (d: { blob_count: number }) => count(d.blob_count),
            },
            {
              key: "blob_bytes",
              label: "Stored",
              align: "right",
              render: (d: { blob_bytes: number }) => bytes(d.blob_bytes),
            },
          ]}
          rows={s.domains}
        />
      )}
    </>
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
          description="Marks console features that depend on optional management API endpoints."
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
