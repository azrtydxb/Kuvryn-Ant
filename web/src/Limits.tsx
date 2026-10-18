import { useState } from "react";
import { requireETag, type Client } from "./api";
import { useAllFeeds } from "./data";
import { useOptionalCall } from "./gaps";
import type { QuotaRule, RateLimitRule } from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Modal,
  PropertyList,
  Select,
  Table,
  Textarea,
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
} from "./ui";

const GIB = 1024 ** 3;
type Draft = { rate_limits: RateLimitRule[]; quotas: QuotaRule[] };
const scopes = [
  { value: "feed", label: "Feed" },
  { value: "namespace", label: "Namespace in a feed" },
  { value: "principal", label: "User or service account" },
  { value: "site", label: "Site (client networks)" },
];

export function LimitsPane({ client }: { client: Client }) {
  const cfg = useOptionalCall(client, "getLimits");
  const usage = useOptionalCall(client, "getLimitsUsage");
  const feeds = useAllFeeds(client);
  const [rule, setRule] = useState<{ index: number; value?: RateLimitRule }>();
  const [quota, setQuota] = useState<{ index: number; value?: QuotaRule }>();
  const action = useAction();
  const toast = useToast();
  if (cfg.state === "loading") return <Loading what="limits and quotas" />;
  if (cfg.state === "unavailable")
    return <Unavailable what="Rate limits and quotas" />;
  if (cfg.state === "error") return <Failure error={cfg.error} />;
  const slug = (id: string) => feeds.items.find((f) => f.id === id)?.slug ?? id;
  const subject = (r: RateLimitRule) =>
    r.scope === "feed" || r.scope === "namespace" ? slug(r.subject) : r.subject;
  const current: Draft = {
    rate_limits: cfg.data.rate_limits,
    quotas: cfg.data.quotas,
  };
  async function save(next: Draft, done: string) {
    const ok = await action.run(() =>
      client.call("putLimits", {
        etag: requireETag(`"${cfg.state === "ready" ? cfg.data.revision : 0}"`),
        body: next,
      }),
    );
    if (ok) {
      toast(done);
      setRule(undefined);
      setQuota(undefined);
      if (cfg.state === "ready") cfg.reload();
      if (usage.state !== "loading") usage.reload();
    }
    return ok;
  }
  const used = (q: QuotaRule) =>
    usage.state === "ready"
      ? usage.data.quotas.find(
          (u) =>
            u.scope === q.scope &&
            u.feed_id === q.feed_id &&
            (u.namespace ?? "") === (q.namespace ?? ""),
        )
      : undefined;
  return (
    <>
      <Alert tone="info" title="Limits apply per instance">
        Request limits are held in memory by each service instance, so several
        instances behind one load balancer each enforce the configured rate.
        Storage quotas are exact: they are checked when a publication commits.
      </Alert>
      <div className="section-head">
        <h2>Request rate limits</h2>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() => setRule({ index: -1 })}
        >
          Add limit
        </Button>
      </div>
      {current.rate_limits.length ? (
        <Table
          onRowClick={(r: RateLimitRule) =>
            setRule({ index: current.rate_limits.indexOf(r), value: r })
          }
          columns={[
            {
              key: "scope",
              label: "Applies to",
              render: (r: RateLimitRule) =>
                scopes.find((s) => s.value === r.scope)?.label,
            },
            {
              key: "subject",
              label: "Subject",
              render: (r: RateLimitRule) => <Mono>{subject(r)}</Mono>,
            },
            {
              key: "namespace",
              label: "Namespace",
              render: (r: RateLimitRule) => r.namespace ?? "—",
            },
            {
              key: "cidrs",
              label: "Networks",
              render: (r: RateLimitRule) =>
                r.scope === "site"
                  ? r.cidrs?.length
                    ? r.cidrs.join(", ")
                    : "all clients"
                  : "—",
            },
            {
              key: "requests_per_minute",
              label: "Requests / min",
              align: "right",
            },
            { key: "burst", label: "Burst", align: "right" },
          ]}
          rows={current.rate_limits}
        />
      ) : (
        <p className="muted">No request limits. Traffic is not throttled.</p>
      )}
      {usage.state === "ready" && (
        <PropertyList
          items={[
            {
              label: "Throttled since start",
              value: (["principal", "feed", "namespace", "site"] as const)
                .map((k) => `${k} ${count(usage.data.throttled[k])}`)
                .join(" · "),
            },
          ]}
        />
      )}
      <div className="section-head">
        <h2>Storage quotas</h2>
        <Button
          size="sm"
          variant="secondary"
          icon="plus"
          onClick={() => setQuota({ index: -1 })}
        >
          Add quota
        </Button>
      </div>
      {current.quotas.length ? (
        <Table
          onRowClick={(q: QuotaRule) =>
            setQuota({ index: current.quotas.indexOf(q), value: q })
          }
          columns={[
            {
              key: "feed_id",
              label: "Feed",
              render: (q: QuotaRule) => <Mono>{slug(q.feed_id)}</Mono>,
            },
            {
              key: "namespace",
              label: "Namespace",
              render: (q: QuotaRule) => q.namespace ?? "whole feed",
            },
            {
              key: "bytes",
              label: "Size",
              align: "right",
              render: (q: QuotaRule) => {
                const u = used(q);
                const full =
                  u && q.max_bytes != null && u.bytes_used >= q.max_bytes * 0.9;
                return (
                  <span className="row tight">
                    {u ? bytes(u.bytes_used) : "—"} of{" "}
                    {q.max_bytes != null ? bytes(q.max_bytes) : "unlimited"}
                    {full && <Badge tone="warn">near limit</Badge>}
                  </span>
                );
              },
            },
            {
              key: "packages",
              label: "Packages",
              align: "right",
              render: (q: QuotaRule) => {
                const u = used(q);
                return `${u ? count(u.packages_used) : "—"} of ${
                  q.max_packages != null ? count(q.max_packages) : "unlimited"
                }`;
              },
            },
          ]}
          rows={current.quotas}
        />
      ) : (
        <p className="muted">No quotas. Feeds can grow without bound.</p>
      )}
      {rule && (
        <RuleModal
          initial={rule.value}
          feeds={feeds.items}
          action={action}
          onClose={() => setRule(undefined)}
          onSave={(value) => {
            const list = [...current.rate_limits];
            if (rule.index >= 0) list[rule.index] = value;
            else list.push(value);
            return save({ ...current, rate_limits: list }, "Limit saved");
          }}
          onRemove={
            rule.index >= 0
              ? () =>
                  save(
                    {
                      ...current,
                      rate_limits: current.rate_limits.filter(
                        (_, i) => i !== rule.index,
                      ),
                    },
                    "Limit removed",
                  )
              : undefined
          }
        />
      )}
      {quota && (
        <QuotaModal
          initial={quota.value}
          feeds={feeds.items}
          action={action}
          onClose={() => setQuota(undefined)}
          onSave={(value) => {
            const list = [...current.quotas];
            if (quota.index >= 0) list[quota.index] = value;
            else list.push(value);
            return save({ ...current, quotas: list }, "Quota saved");
          }}
          onRemove={
            quota.index >= 0
              ? () =>
                  save(
                    {
                      ...current,
                      quotas: current.quotas.filter(
                        (_, i) => i !== quota.index,
                      ),
                    },
                    "Quota removed",
                  )
              : undefined
          }
        />
      )}
    </>
  );
}

type ActionState = ReturnType<typeof useAction>;
type FeedRef = { id: string; slug: string };

function RuleModal({
  initial,
  feeds,
  action,
  onClose,
  onSave,
  onRemove,
}: {
  initial?: RateLimitRule;
  feeds: FeedRef[];
  action: ActionState;
  onClose: () => void;
  onSave: (r: RateLimitRule) => Promise<boolean>;
  onRemove?: () => Promise<boolean>;
}) {
  const [scope, setScope] = useState<string>(initial?.scope ?? "feed");
  const [feed, setFeed] = useState(
    initial && (initial.scope === "feed" || initial.scope === "namespace")
      ? initial.subject
      : (feeds[0]?.id ?? ""),
  );
  const [text, setText] = useState(
    initial && (initial.scope === "principal" || initial.scope === "site")
      ? initial.subject
      : "",
  );
  const [namespace, setNamespace] = useState(initial?.namespace ?? "");
  const [cidrs, setCidrs] = useState((initial?.cidrs ?? []).join("\n"));
  const [rate, setRate] = useState(String(initial?.requests_per_minute ?? 600));
  const [burst, setBurst] = useState(String(initial?.burst ?? 100));
  const feedScoped = scope === "feed" || scope === "namespace";
  const valid =
    Number(rate) >= 1 &&
    Number(burst) >= 1 &&
    (feedScoped ? !!feed : !!text) &&
    (scope !== "namespace" || !!namespace);
  const build = (): RateLimitRule => ({
    scope: scope as RateLimitRule["scope"],
    subject: feedScoped ? feed : text.trim(),
    ...(scope === "namespace" ? { namespace } : {}),
    ...(scope === "site"
      ? {
          cidrs: cidrs
            .split(/[\s,]+/)
            .map((c) => c.trim())
            .filter(Boolean),
        }
      : {}),
    requests_per_minute: Number(rate),
    burst: Number(burst),
  });
  return (
    <Modal
      title={initial ? "Edit request limit" : "Add request limit"}
      description="Requests above the rate (after the burst allowance) receive HTTP 429 with a Retry-After header."
      onClose={onClose}
      width={600}
      actions={
        <>
          {onRemove && (
            <Button
              variant="danger"
              icon="trash-2"
              className="push-left"
              disabled={action.busy}
              onClick={() => void onRemove()}
            >
              Remove
            </Button>
          )}
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy || !valid}
            onClick={() => void onSave(build())}
          >
            {action.busy ? "Saving…" : "Save limit"}
          </Button>
        </>
      }
    >
      <Select
        label="Applies to"
        options={scopes}
        value={scope}
        disabled={!!initial}
        onChange={val(setScope)}
      />
      {feedScoped ? (
        <Select
          label="Feed"
          options={feeds.map((f) => ({ value: f.id, label: f.slug }))}
          value={feed}
          disabled={!!initial}
          onChange={val(setFeed)}
        />
      ) : (
        <Input
          label={scope === "site" ? "Site label" : "Principal ID"}
          mono
          value={text}
          disabled={!!initial}
          onChange={val(setText)}
          hint={
            scope === "site"
              ? "A name for this group of clients, for example build-farm."
              : "The identifier of the user or service account."
          }
        />
      )}
      {scope === "namespace" && (
        <Input
          label="Namespace"
          mono
          value={namespace}
          disabled={!!initial}
          onChange={val(setNamespace)}
          hint="The first path segment below the feed, such as an npm scope."
        />
      )}
      {scope === "site" && (
        <Textarea
          label="Client networks (CIDR)"
          mono
          value={cidrs}
          onChange={val(setCidrs)}
          hint="One per line. Leave empty to limit all clients together."
        />
      )}
      <div className="cols">
        <Input
          label="Requests per minute"
          type="number"
          mono
          min={1}
          value={rate}
          onChange={val(setRate)}
        />
        <Input
          label="Burst"
          type="number"
          mono
          min={1}
          value={burst}
          onChange={val(setBurst)}
        />
      </div>
      <Failure error={action.error} />
    </Modal>
  );
}

function QuotaModal({
  initial,
  feeds,
  action,
  onClose,
  onSave,
  onRemove,
}: {
  initial?: QuotaRule;
  feeds: FeedRef[];
  action: ActionState;
  onClose: () => void;
  onSave: (q: QuotaRule) => Promise<boolean>;
  onRemove?: () => Promise<boolean>;
}) {
  const [feed, setFeed] = useState(initial?.feed_id ?? feeds[0]?.id ?? "");
  const [namespace, setNamespace] = useState(initial?.namespace ?? "");
  const [size, setSize] = useState(
    initial?.max_bytes != null ? String(initial.max_bytes / GIB) : "",
  );
  const [packages, setPackages] = useState(
    initial?.max_packages != null ? String(initial.max_packages) : "",
  );
  const valid = !!feed && (size !== "" || packages !== "");
  const build = (): QuotaRule => ({
    scope: namespace ? "namespace" : "feed",
    feed_id: feed,
    ...(namespace ? { namespace } : {}),
    ...(size !== "" ? { max_bytes: Math.round(Number(size) * GIB) } : {}),
    ...(packages !== "" ? { max_packages: Number(packages) } : {}),
  });
  return (
    <Modal
      title={initial ? "Edit storage quota" : "Add storage quota"}
      description="A publication that would exceed a quota is rejected and leaves nothing behind. Sizes count logical bytes."
      onClose={onClose}
      width={600}
      actions={
        <>
          {onRemove && (
            <Button
              variant="danger"
              icon="trash-2"
              className="push-left"
              disabled={action.busy}
              onClick={() => void onRemove()}
            >
              Remove
            </Button>
          )}
          <Button variant="secondary" onClick={onClose}>
            Cancel
          </Button>
          <Button
            disabled={action.busy || !valid}
            onClick={() => void onSave(build())}
          >
            {action.busy ? "Saving…" : "Save quota"}
          </Button>
        </>
      }
    >
      <Select
        label="Feed"
        options={feeds.map((f) => ({ value: f.id, label: f.slug }))}
        value={feed}
        disabled={!!initial}
        onChange={val(setFeed)}
      />
      <Input
        label="Namespace (optional)"
        mono
        value={namespace}
        disabled={!!initial}
        onChange={val(setNamespace)}
        hint="Leave empty to bound the whole feed."
      />
      <div className="cols">
        <Input
          label="Max size (GiB)"
          type="number"
          mono
          min={0}
          step="any"
          value={size}
          onChange={val(setSize)}
        />
        <Input
          label="Max packages"
          type="number"
          mono
          min={0}
          value={packages}
          onChange={val(setPackages)}
        />
      </div>
      <Failure error={action.error} />
    </Modal>
  );
}
