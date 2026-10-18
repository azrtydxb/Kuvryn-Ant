import { useMemo, useState } from "react";
import { requireETag, type Client } from "./api";
import { isUnavailable } from "./gaps";
import { FeedTemplates } from "./Templates";
import type {
  Feed,
  IdentityPrincipal,
  PublishPolicy,
  PublishPolicyTestResult,
  PublishRule,
  SourceAttachmentPage,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Icon,
  Input,
  Modal,
  SegmentedControl,
  Select,
  Table,
} from "./ds/bundle";
import {
  Card,
  Failure,
  Loading,
  Mono,
  Unavailable,
  useAction,
  useResource,
  useToast,
  val,
} from "./ui";

type Attachment = SourceAttachmentPage["items"][number];
const ruleIDPattern = /^[A-Za-z0-9_.:-]{1,128}$/;
const selectorKeys = [
  ["namespace", "Namespace", 256],
  ["package", "Package", 256],
  ["path_prefix", "Path prefix", 2048],
] as const;

export function PolicyTab({ client, feed }: { client: Client; feed: Feed }) {
  const policy = useResource(
    () => client.call("getPublishPolicy", { params: { id: feed.id } }),
    [client, feed.id],
  );
  const attached = useResource(
    () =>
      client.call("listFeedSources", {
        params: { id: feed.id },
        query: { limit: "200" },
      }),
    [client, feed.id],
  );
  const principals = useResource(
    () => client.call("listPrincipals", { query: { limit: "200" } }),
    [client],
  );
  return (
    <>
      <Failure error={policy.error ?? attached.error} />
      {(policy.loading || attached.loading) && (
        <Loading what="publish policy" />
      )}
      {policy.value && attached.value && (
        <PolicyEditor
          client={client}
          policy={policy.value.data}
          etag={policy.value.etag}
          attached={attached.value.data.items}
          principals={principals.value?.data.items}
          reload={policy.reload}
        />
      )}
      {policy.value && (
        <FeedTemplates client={client} feed={feed} changed={policy.reload} />
      )}
    </>
  );
}

function PolicyEditor({
  client,
  policy,
  etag,
  attached,
  principals,
  reload,
}: {
  client: Client;
  policy: PublishPolicy;
  etag: string | null;
  attached: Attachment[];
  principals?: IdentityPrincipal[];
  reload: () => void;
}) {
  const [defaultID, setDefaultID] = useState(policy.default_source_id ?? "");
  const [rules, setRules] = useState<PublishRule[]>(policy.rules);
  const [edit, setEdit] = useState<{ rule: PublishRule; index: number }>();
  const action = useAction();
  const toast = useToast();
  const locals = useMemo(
    () =>
      attached.filter((a) => a.source.kind === "local").map((a) => a.source),
    [attached],
  );
  const nameOf = (id?: string) =>
    locals.find((s) => s.id === id)?.name ??
    attached.find((a) => a.source.id === id)?.source.name ??
    id ??
    "";
  const dirty =
    defaultID !== (policy.default_source_id ?? "") ||
    JSON.stringify(rules) !== JSON.stringify(policy.rules);
  const sorted = rules
    .map((rule, index) => ({ rule, index }))
    .sort((a, b) => b.rule.priority - a.rule.priority);
  async function save() {
    const ok = await action.run(async () => {
      if (new Set(rules.map((r) => r.id)).size !== rules.length)
        throw Error("Rule IDs must be unique.");
      if (rules.some((r) => !Number.isSafeInteger(r.priority)))
        throw Error("Rule priorities must be safe integers.");
      if (rules.some((r) => !r.deny && !r.target_source_id))
        throw Error("Every allow rule needs a target source.");
      await client.call("setPublishPolicy", {
        params: { id: policy.feed_id },
        etag: requireETag(etag),
        body: { default_source_id: defaultID, rules },
      });
    });
    if (ok) {
      toast("Publish policy saved", `Replaced ${rules.length} rules`);
      reload();
    }
  }
  return (
    <div className="split">
      <Card
        flush
        className="grow2"
        title="Rules"
        actions={
          <Button
            size="sm"
            variant="secondary"
            icon="plus"
            disabled={rules.length >= 256}
            onClick={() =>
              setEdit({
                index: -1,
                rule: {
                  id: "rule-" + crypto.randomUUID().slice(0, 8),
                  priority: 0,
                  deny: false,
                  selector: {},
                  target_source_id: locals[0]?.id,
                },
              })
            }
          >
            Add rule
          </Button>
        }
      >
        <p className="card__note">
          Deny rules always win. Otherwise the highest-priority match picks the
          target; unmatched writes use the default, or are rejected when there
          is none. Saving replaces the complete policy.
        </p>
        {sorted.length ? (
          <Table
            onRowClick={(r: { rule: PublishRule; index: number }) => setEdit(r)}
            rowKey="index"
            className="flat"
            columns={[
              {
                key: "priority",
                label: "Priority",
                mono: true,
                width: 90,
                render: (r: { rule: PublishRule }) => r.rule.priority,
              },
              {
                key: "id",
                label: "Rule",
                mono: true,
                render: (r: { rule: PublishRule }) => r.rule.id,
              },
              {
                key: "when",
                label: "When",
                render: (r: { rule: PublishRule }) => {
                  const parts = Object.entries(r.rule.selector)
                    .filter(([, v]) => v)
                    .map(([k, v]) => `${k.replace("_", " ")} = ${v}`);
                  return (
                    <span className="small">
                      {parts.join(" · ") || "Any write"}
                    </span>
                  );
                },
              },
              {
                key: "then",
                label: "Then",
                render: (r: { rule: PublishRule }) =>
                  r.rule.deny ? (
                    <Badge tone="bad" icon="ban">
                      Reject
                    </Badge>
                  ) : (
                    <span className="row tight">
                      <Icon name="arrow-right" size={13} />
                      <Mono>{nameOf(r.rule.target_source_id) || "—"}</Mono>
                    </span>
                  ),
              },
            ]}
            rows={sorted}
          />
        ) : (
          <p className="pad muted">
            No rules. Every write uses the default destination.
          </p>
        )}
        <div className="savebar savebar--always">
          <div className="row">
            <Select
              size="sm"
              aria-label="Default write target"
              options={[
                ...locals.map((s) => ({ value: s.id, label: s.name })),
                { value: "", label: "Reject unmatched writes" },
              ]}
              value={defaultID}
              onChange={val(setDefaultID)}
            />
            <span className="muted small">
              Default destination:{" "}
              <strong>
                {defaultID ? nameOf(defaultID) : "Reject unmatched writes"}
              </strong>
            </span>
          </div>
          <div className="row">
            {dirty && (
              <Button
                size="sm"
                variant="secondary"
                onClick={() => {
                  setRules(policy.rules);
                  setDefaultID(policy.default_source_id ?? "");
                }}
              >
                Discard
              </Button>
            )}
            <Button size="sm" disabled={action.busy} onClick={save}>
              {action.busy ? "Saving…" : "Save publish policy"}
            </Button>
          </div>
        </div>
        <Failure error={action.error} className="pad-alert" />
        <p className="card__note">
          The server validates ambiguous matches and target permissions before
          applying any change. Every target must be an attached, local source of
          this feed's format.
        </p>
      </Card>
      <PolicyTest
        client={client}
        feedId={policy.feed_id}
        defaultID={defaultID}
        rules={rules}
        principals={principals}
      />
      {edit && (
        <RuleModal
          initial={edit.rule}
          isNew={edit.index < 0}
          locals={locals}
          principals={principals}
          onClose={() => setEdit(undefined)}
          onRemove={() => {
            setRules(rules.filter((_, i) => i !== edit.index));
            setEdit(undefined);
          }}
          onApply={(rule) => {
            setRules(
              edit.index < 0
                ? [...rules, rule]
                : rules.map((r, i) => (i === edit.index ? rule : r)),
            );
            setEdit(undefined);
          }}
          taken={rules.filter((_, i) => i !== edit.index).map((r) => r.id)}
        />
      )}
    </div>
  );
}

function RuleModal({
  initial,
  isNew,
  locals,
  principals,
  taken,
  onClose,
  onRemove,
  onApply,
}: {
  initial: PublishRule;
  isNew: boolean;
  locals: { id: string; name: string }[];
  principals?: IdentityPrincipal[];
  taken: string[];
  onClose: () => void;
  onRemove: () => void;
  onApply: (r: PublishRule) => void;
}) {
  const [rule, setRule] = useState(initial);
  const [priority, setPriority] = useState(String(initial.priority));
  const idOK = ruleIDPattern.test(rule.id) && !taken.includes(rule.id);
  const prio = Number(priority);
  const prioOK = priority !== "" && Number.isSafeInteger(prio);
  const targetOK = rule.deny || !!rule.target_source_id;
  const setSel = (key: keyof PublishRule["selector"], value: string) =>
    setRule({
      ...rule,
      selector: { ...rule.selector, [key]: value || undefined },
    });
  return (
    <Modal
      title={isNew ? "Add rule" : "Edit rule"}
      description="All populated selectors must match; empty fields match any value. Matches are exact and case-sensitive."
      onClose={onClose}
      width={600}
      actions={
        <>
          {!isNew && (
            <Button
              variant="danger"
              icon="trash-2"
              className="push-left"
              onClick={onRemove}
            >
              Remove rule
            </Button>
          )}
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={!idOK || !prioOK || !targetOK}
            onClick={() => {
              const next = { ...rule, priority: prio };
              if (next.deny) delete next.target_source_id;
              onApply(next);
            }}
          >
            Apply
          </Button>
        </>
      }
    >
      <div className="cols cols--21">
        <Input
          label="Rule ID"
          mono
          required
          maxLength={128}
          value={rule.id}
          onChange={(e: { target: { value: string } }) =>
            setRule({ ...rule, id: e.target.value })
          }
          error={
            idOK
              ? undefined
              : "Unique; letters, digits, _ . : - up to 128 characters."
          }
        />
        <Input
          label="Match priority"
          type="number"
          mono
          required
          value={priority}
          onChange={val(setPriority)}
          hint="Highest wins"
        />
      </div>
      <div className="cols">
        {selectorKeys.map(([key, label, max]) => (
          <Input
            key={key}
            label={label}
            mono
            placeholder="any"
            maxLength={max}
            value={rule.selector[key] ?? ""}
            onChange={(e: { target: { value: string } }) =>
              setSel(key, e.target.value)
            }
          />
        ))}
        {principals ? (
          <Select
            label="Principal"
            options={[
              { value: "", label: "Any principal" },
              ...principals.map((p) => ({
                value: p.id,
                label: `${p.subject} (${p.kind})`,
              })),
            ]}
            value={rule.selector.principal_id ?? ""}
            onChange={(e: { target: { value: string } }) =>
              setSel("principal_id", e.target.value)
            }
          />
        ) : (
          <Input
            label="Principal ID"
            mono
            placeholder="any"
            maxLength={256}
            value={rule.selector.principal_id ?? ""}
            onChange={(e: { target: { value: string } }) =>
              setSel("principal_id", e.target.value)
            }
            hint="Identity listing is not permitted for you; enter the ID."
          />
        )}
      </div>
      <SegmentedControl
        block
        aria-label="Rule effect"
        options={[
          { value: "allow", label: "Publish to source", icon: "arrow-right" },
          { value: "deny", label: "Reject write", icon: "ban" },
        ]}
        value={rule.deny ? "deny" : "allow"}
        onChange={(v: string) =>
          setRule({
            ...rule,
            deny: v === "deny",
            target_source_id:
              v === "deny"
                ? undefined
                : (rule.target_source_id ?? locals[0]?.id),
          })
        }
      />
      {!rule.deny &&
        (locals.length ? (
          <Select
            label="Target source"
            options={locals.map((s) => ({ value: s.id, label: s.name }))}
            value={rule.target_source_id ?? ""}
            onChange={val((v) => setRule({ ...rule, target_source_id: v }))}
            hint="Only attached local sources of this format."
          />
        ) : (
          <Alert tone="warn" title="No attached local source">
            Attach a local {"source"} on the Read order tab before adding an
            allow rule, or use a reject rule.
          </Alert>
        ))}
      <p className="muted small">
        Matched destination:{" "}
        <strong>
          {rule.deny
            ? "Reject write"
            : (locals.find((s) => s.id === rule.target_source_id)?.name ??
              "Select a local source")}
        </strong>
      </p>
    </Modal>
  );
}

function PolicyTest({
  client,
  feedId,
  defaultID,
  rules,
  principals,
}: {
  client: Client;
  feedId: string;
  defaultID: string;
  rules: PublishRule[];
  principals?: IdentityPrincipal[];
}) {
  const [coordinate, setCoordinate] = useState("");
  const [principal, setPrincipal] = useState("");
  const [result, setResult] = useState<PublishPolicyTestResult>();
  const [unavailable, setUnavailable] = useState(false);
  const action = useAction();
  return (
    <Card title="Test a publish" className="grow1">
      <p className="muted small">Dry-run a write against the unsaved rules.</p>
      <form
        className="stack"
        onSubmit={async (e) => {
          e.preventDefault();
          setResult(undefined);
          setUnavailable(false);
          await action.run(async () => {
            try {
              const res = await client.call("testPublishPolicy", {
                params: { id: feedId },
                body: {
                  policy: { default_source_id: defaultID || undefined, rules },
                  coordinate,
                  principal_id: principal || undefined,
                },
              });
              setResult(res.data);
            } catch (err) {
              if (isUnavailable(err)) setUnavailable(true);
              else throw err;
            }
          });
        }}
      >
        <Input
          size="sm"
          mono
          label="Package"
          placeholder="platform/gateway:v2.0.0"
          required
          maxLength={512}
          value={coordinate}
          onChange={val(setCoordinate)}
        />
        {principals ? (
          <Select
            size="sm"
            label="As principal"
            options={[
              { value: "", label: "Current principal" },
              ...principals.map((p) => ({ value: p.id, label: p.subject })),
            ]}
            value={principal}
            onChange={val(setPrincipal)}
          />
        ) : null}
        <Button
          type="submit"
          size="sm"
          variant="secondary"
          disabled={action.busy}
        >
          {action.busy ? "Testing…" : "Run test"}
        </Button>
      </form>
      {unavailable && <Unavailable what="Policy dry-run" compact />}
      <Failure error={action.error} />
      {result && (
        <Alert
          tone={result.allowed ? "good" : "bad"}
          title={result.allowed ? "Allowed" : "Rejected"}
        >
          {result.matched_rule_id && (
            <>
              Matched <Mono>{result.matched_rule_id}</Mono>.{" "}
            </>
          )}
          {result.target_source_name && (
            <>
              Target <Mono>{result.target_source_name}</Mono>.{" "}
            </>
          )}
          {result.reason} {result.denial}
        </Alert>
      )}
    </Card>
  );
}
