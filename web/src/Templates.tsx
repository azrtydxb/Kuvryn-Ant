import { useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllSources } from "./data";
import { useOptionalCall } from "./gaps";
import type {
  Feed,
  PolicyTemplate,
  PolicyTemplateInput,
  PublishRule,
  ReadPolicySettings,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  IconButton,
  Input,
  Modal,
  PropertyList,
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
} from "./ui";

const kinds = [
  { value: "publish", label: "Publish policy" },
  { value: "read", label: "Read policy" },
];
const tri = [
  { value: "", label: "Inherit" },
  { value: "true", label: "Yes" },
  { value: "false", label: "No" },
];
const readFields = [
  ["local_first", "Local sources first"],
  ["fallback_on_not_found", "Fall back when not found"],
  ["failover_on_upstream_error", "Fail over on upstream error"],
] as const;

export function TemplatesPane({ client }: { client: Client }) {
  const list = useOptionalCall(client, "listPolicyTemplates");
  const sources = useAllSources(client);
  const [editing, setEditing] = useState<PolicyTemplate | "new">();
  if (list.state === "loading") return <Loading what="policy templates" />;
  if (list.state === "unavailable")
    return <Unavailable what="Policy templates" />;
  if (list.state === "error") return <Failure error={list.error} />;
  const items = list.data.items;
  return (
    <>
      <Alert tone="info" title="Reusable policy">
        A feed references a template and inherits its rules. The feed's own
        publish policy is applied on top, and the feed's Policy tab shows the
        effective result. Templates may extend one template of the same kind;
        cycles are rejected.
      </Alert>
      <div className="section-head">
        <h2>Templates</h2>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() => setEditing("new")}
        >
          Add template
        </Button>
      </div>
      {items.length ? (
        <Table
          onRowClick={(t: PolicyTemplate) => setEditing(t)}
          columns={[
            {
              key: "name",
              label: "Name",
              render: (t: PolicyTemplate) => <Mono>{t.name}</Mono>,
            },
            {
              key: "kind",
              label: "Kind",
              render: (t: PolicyTemplate) => (
                <Badge tone={t.kind === "publish" ? "info" : "neutral"}>
                  {t.kind}
                </Badge>
              ),
            },
            {
              key: "extends_id",
              label: "Extends",
              render: (t: PolicyTemplate) =>
                items.find((x) => x.id === t.extends_id)?.name ?? "—",
            },
            {
              key: "bound_feeds",
              label: "Feeds",
              align: "right",
              render: (t: PolicyTemplate) => count(t.bound_feeds),
            },
            { key: "description", label: "Description" },
          ]}
          rows={items}
        />
      ) : (
        <p className="muted">No templates yet.</p>
      )}
      {editing && (
        <TemplateModal
          client={client}
          template={editing === "new" ? undefined : editing}
          all={items}
          sources={sources.items.filter((s) => s.kind === "local")}
          onClose={() => setEditing(undefined)}
          done={() => {
            setEditing(undefined);
            list.reload();
          }}
        />
      )}
    </>
  );
}

type LocalSource = { id: string; name: string };

function TemplateModal({
  client,
  template,
  all,
  sources,
  onClose,
  done,
}: {
  client: Client;
  template?: PolicyTemplate;
  all: PolicyTemplate[];
  sources: LocalSource[];
  onClose: () => void;
  done: () => void;
}) {
  const [name, setName] = useState(template?.name ?? "");
  const [description, setDescription] = useState(template?.description ?? "");
  const [kind, setKind] = useState<string>(template?.kind ?? "publish");
  const [extendsID, setExtendsID] = useState(template?.extends_id ?? "");
  const [defaultID, setDefaultID] = useState(
    template?.publish?.default_source_id ?? "",
  );
  const [rules, setRules] = useState<PublishRule[]>(
    template?.publish?.rules ?? [],
  );
  const [read, setRead] = useState<ReadPolicySettings>(template?.read ?? {});
  const action = useAction();
  const toast = useToast();
  const parents = all.filter((t) => t.kind === kind && t.id !== template?.id);
  const valid = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$/.test(name);
  async function save() {
    const body: PolicyTemplateInput = {
      name,
      description,
      kind: kind as PolicyTemplateInput["kind"],
      ...(extendsID ? { extends_id: extendsID } : {}),
      ...(kind === "publish"
        ? {
            publish: {
              default_source_id: defaultID,
              rules: rules.map(cleanRule),
            },
          }
        : { read }),
    };
    const ok = await action.run(async () => {
      if (new Set(rules.map((r) => r.id)).size !== rules.length)
        throw Error("Rule IDs must be unique.");
      if (rules.some((r) => !r.deny && !r.target_source_id))
        throw Error("Every allow rule needs a target source.");
      if (template)
        await client.call("putPolicyTemplate", {
          params: { id: template.id },
          etag: requireETag(`"${template.revision}"`),
          body,
        });
      else await client.call("createPolicyTemplate", { body });
    });
    if (ok) {
      toast("Template saved");
      done();
    }
  }
  return (
    <Modal
      title={template ? `Edit template ${template.name}` : "Add template"}
      description="Saving revalidates every feed that uses this template; the change is rejected if any would become invalid."
      onClose={onClose}
      width={760}
      actions={
        <>
          {template && (
            <Button
              variant="danger"
              icon="trash-2"
              className="push-left"
              disabled={action.busy}
              onClick={async () => {
                const ok = await action.run(() =>
                  client.call("deletePolicyTemplate", {
                    params: { id: template.id },
                    etag: requireETag(`"${template.revision}"`),
                  }),
                );
                if (ok) {
                  toast("Template removed");
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
          <Button disabled={action.busy || !valid} onClick={() => void save()}>
            {action.busy ? "Saving…" : "Save template"}
          </Button>
        </>
      }
    >
      <div className="cols">
        <Input
          label="Name"
          mono
          value={name}
          onChange={val(setName)}
          error={
            name && !valid ? "Letters, digits, . _ - (max 63)." : undefined
          }
        />
        <Select
          label="Kind"
          options={kinds}
          value={kind}
          disabled={!!template}
          onChange={val((v) => {
            setKind(v);
            setExtendsID("");
          })}
        />
      </div>
      <Input
        label="Description"
        value={description}
        onChange={val(setDescription)}
      />
      <Select
        label="Extends"
        options={[
          { value: "", label: "Nothing" },
          ...parents.map((t) => ({ value: t.id, label: t.name })),
        ]}
        value={extendsID}
        onChange={val(setExtendsID)}
      />
      {kind === "publish" ? (
        <>
          <Select
            label="Default target source"
            options={[
              { value: "", label: "None (rules decide)" },
              ...sources.map((s) => ({ value: s.id, label: s.name })),
            ]}
            value={defaultID}
            onChange={val(setDefaultID)}
          />
          <RulesEditor rules={rules} setRules={setRules} sources={sources} />
        </>
      ) : (
        <div className="cols">
          {readFields.map(([key, label]) => (
            <Select
              key={key}
              label={label}
              options={tri}
              value={read[key] == null ? "" : String(read[key])}
              onChange={val((v) =>
                setRead((r) => {
                  const next = { ...r };
                  if (v === "") delete next[key];
                  else next[key] = v === "true";
                  return next;
                }),
              )}
            />
          ))}
        </div>
      )}
      <Failure error={action.error} />
    </Modal>
  );
}

function cleanRule(r: PublishRule): PublishRule {
  const selector = Object.fromEntries(
    Object.entries(r.selector).filter(([, v]) => v),
  );
  return {
    id: r.id,
    priority: r.priority,
    selector,
    deny: r.deny,
    ...(r.deny || !r.target_source_id
      ? {}
      : { target_source_id: r.target_source_id }),
  };
}

function RulesEditor({
  rules,
  setRules,
  sources,
}: {
  rules: PublishRule[];
  setRules: (r: PublishRule[]) => void;
  sources: LocalSource[];
}) {
  const update = (i: number, patch: Partial<PublishRule>) =>
    setRules(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  const selector = (i: number, key: string, value: string) =>
    update(i, { selector: { ...rules[i].selector, [key]: value } });
  return (
    <div className="stack">
      <div className="section-head">
        <h3>Publish rules</h3>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() =>
            setRules([
              ...rules,
              {
                id: `rule-${rules.length + 1}`,
                priority: 0,
                selector: {},
                deny: false,
              },
            ])
          }
        >
          Add rule
        </Button>
      </div>
      {rules.length === 0 && (
        <p className="muted small">
          No rules. Publishes use the default target source.
        </p>
      )}
      {rules.map((r, i) => (
        <div key={i} className="cols" aria-label={`Rule ${r.id}`}>
          <Input
            label="Rule ID"
            mono
            value={r.id}
            onChange={val((v) => update(i, { id: v }))}
          />
          <Input
            label="Priority"
            type="number"
            mono
            value={String(r.priority)}
            onChange={val((v) => update(i, { priority: Number(v) }))}
          />
          <Input
            label="Namespace"
            mono
            value={r.selector.namespace ?? ""}
            onChange={val((v) => selector(i, "namespace", v))}
          />
          <Input
            label="Package"
            mono
            value={r.selector.package ?? ""}
            onChange={val((v) => selector(i, "package", v))}
          />
          <Input
            label="Path prefix"
            mono
            value={r.selector.path_prefix ?? ""}
            onChange={val((v) => selector(i, "path_prefix", v))}
          />
          <Select
            label="Target"
            disabled={r.deny}
            options={[
              { value: "", label: r.deny ? "Denied" : "Choose…" },
              ...sources.map((s) => ({ value: s.id, label: s.name })),
            ]}
            value={r.target_source_id ?? ""}
            onChange={val((v) => update(i, { target_source_id: v }))}
          />
          <Switch
            label="Deny"
            labelPosition="end"
            checked={r.deny}
            onChange={chk((v) => update(i, { deny: v }))}
          />
          <IconButton
            icon="trash-2"
            label={`Remove rule ${r.id}`}
            onClick={() => setRules(rules.filter((_, j) => j !== i))}
          />
        </div>
      ))}
    </div>
  );
}

// Templates bound to one feed and the resulting effective configuration, shown
// on the feed's Policy tab. Renders nothing on a server without templates.
export function FeedTemplates({
  client,
  feed,
  changed,
}: {
  client: Client;
  feed: Feed;
  changed: () => void;
}) {
  const binding = useOptionalCall(client, "getFeedPolicyTemplates", {
    params: { id: feed.id },
  });
  const list = useOptionalCall(client, "listPolicyTemplates");
  const effective = useOptionalCall(client, "getEffectivePolicy", {
    params: { id: feed.id },
  });
  const [publish, setPublish] = useState<string>();
  const [read, setRead] = useState<string>();
  const action = useAction();
  const toast = useToast();
  if (
    binding.state === "unavailable" ||
    list.state === "unavailable" ||
    effective.state === "unavailable"
  )
    return null;
  if (binding.state === "loading" || list.state === "loading")
    return <Loading what="policy templates" />;
  if (binding.state === "error") return <Failure error={binding.error} />;
  if (list.state === "error") return <Failure error={list.error} />;
  const b = binding.data;
  const pub = publish ?? b.publish_template_id ?? "";
  const rd = read ?? b.read_template_id ?? "";
  const options = (kind: string) => [
    { value: "", label: "None" },
    ...list.data.items
      .filter((t) => t.kind === kind)
      .map((t) => ({ value: t.id, label: t.name })),
  ];
  const dirty =
    pub !== (b.publish_template_id ?? "") || rd !== (b.read_template_id ?? "");
  async function save() {
    const ok = await action.run(() =>
      client.call("putFeedPolicyTemplates", {
        params: { id: feed.id },
        etag: requireETag(`"${b.revision}"`),
        body: {
          ...(pub ? { publish_template_id: pub } : {}),
          ...(rd ? { read_template_id: rd } : {}),
        },
      }),
    );
    if (ok) {
      toast("Templates applied");
      setPublish(undefined);
      setRead(undefined);
      binding.reload();
      effective.reload();
      changed();
    }
  }
  const e = effective.state === "ready" ? effective.data : undefined;
  return (
    <>
      <div className="section-head">
        <h2>Templates</h2>
        <Button
          size="sm"
          disabled={!dirty || action.busy}
          onClick={() => void save()}
        >
          {action.busy ? "Applying…" : "Apply templates"}
        </Button>
      </div>
      <div className="cols">
        <Select
          label="Publish template"
          options={options("publish")}
          value={pub}
          onChange={val(setPublish)}
        />
        <Select
          label="Read template"
          options={options("read")}
          value={rd}
          onChange={val(setRead)}
        />
      </div>
      <Failure error={action.error} />
      {effective.state === "error" && <Failure error={effective.error} />}
      {e && (
        <>
          <div className="section-head">
            <h2>Effective configuration</h2>
          </div>
          <PropertyList
            items={[
              {
                label: "Publish templates",
                value: e.publish_template_chain.length
                  ? e.publish_template_chain.join(" → ")
                  : "none",
              },
              {
                label: "Default target",
                value: e.default_source_id ? (
                  <span className="row tight">
                    <Mono>{e.default_source_id}</Mono>
                    <Badge>{e.default_origin}</Badge>
                  </span>
                ) : (
                  "none"
                ),
              },
              {
                label: "Read templates",
                value: e.read_template_chain.length
                  ? e.read_template_chain.join(" → ")
                  : "none",
              },
              ...readFields
                .filter(([k]) => e.read[k] != null)
                .map(([k, label]) => ({
                  label,
                  value: (
                    <span className="row tight">
                      {e.read[k] ? "Yes" : "No"}
                      <Badge>{e.read_origins[k]}</Badge>
                    </span>
                  ),
                })),
            ]}
          />
          {e.rules.length > 0 && (
            <Table
              rowKey="id"
              columns={[
                { key: "id", label: "Rule", mono: true },
                { key: "priority", label: "Priority", align: "right" },
                {
                  key: "deny",
                  label: "Action",
                  render: (r: { deny: boolean }) => (r.deny ? "Deny" : "Allow"),
                },
                {
                  key: "origin",
                  label: "From",
                  render: (r: { origin: string }) => <Badge>{r.origin}</Badge>,
                },
              ]}
              rows={e.rules}
            />
          )}
        </>
      )}
    </>
  );
}
