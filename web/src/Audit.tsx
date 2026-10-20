import { useMemo, useState } from "react";
import { requireETag, type Client } from "./api";
import { useNames } from "./data";
import { useResource } from "./ui";
import { useOptionalCall } from "./gaps";
import type { AuditEvent, AuditSink } from "./generated";
import {
  Badge,
  Button,
  EmptyState,
  Icon,
  Input,
  Modal,
  PropertyList,
  Select,
  Switch,
  SegmentedControl,
  Table,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  PageHeading,
  Pager,
  shortId,
  Toolbar,
  Unavailable,
  useAction,
  useDebounced,
  useToast,
  chk,
  val,
  when,
} from "./ui";

function tone(result: string) {
  return result === "success" ? "good" : result === "denied" ? "bad" : "warn";
}

export function AuditPage({ client }: { client: Client }) {
  const [principal, setPrincipal] = useState("");
  const [action, setAction] = useState("");
  const [result, setResult] = useState("all");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [cursor, setCursor] = useState("");
  const [siem, setSiem] = useState(false);
  const dp = useDebounced(principal);
  const da = useDebounced(action);
  const filters: Record<string, string> = {
    ...(dp ? { principal: dp } : {}),
    ...(da ? { action: da } : {}),
    ...(result !== "all" ? { result } : {}),
    ...(from ? { from: new Date(from).toISOString() } : {}),
    ...(to ? { to: new Date(to + "T23:59:59").toISOString() } : {}),
  };
  const r = useOptionalCall(client, "listAuditEvents", {
    query: { limit: "50", ...filters, ...(cursor ? { cursor } : {}) },
  });
  const names = useResource(
    () => client.call("listPrincipals", { query: { limit: "200" } }),
    [client],
  );
  const who = useMemo(
    () =>
      new Map((names.value?.data.items ?? []).map((p) => [p.id, p.subject])),
    [names.value],
  );
  const resourceNames = useNames(client);
  // Tokens are removed by cleanup while their audit rows stay, so a token id that
  // no longer resolves is shown as a deleted token (only when the whole list was seen).
  const tokens = useOptionalCall(client, "listServiceTokens", {
    query: { limit: "200", state: "all" },
  });
  const knownTokens =
    tokens.state === "ready" && !tokens.data.page.has_more
      ? new Set(tokens.data.items.map((t) => t.id))
      : undefined;
  const rows = r.state === "ready" ? r.data.items : [];
  const exporter = useAction();
  const toast = useToast();
  const reset = (set: (v: string) => void) => (v: string) => {
    set(v);
    setCursor("");
  };
  async function exportLog(format: "csv" | "ndjson") {
    const ok = await exporter.run(async () => {
      const text = await client.exportText("/api/v1/audit/export", {
        format,
        ...filters,
      });
      const url = URL.createObjectURL(
        new Blob([text], {
          type: format === "csv" ? "text/csv" : "application/x-ndjson",
        }),
      );
      const a = document.createElement("a");
      a.href = url;
      a.download = `audit-log.${format}`;
      a.click();
      URL.revokeObjectURL(url);
    });
    if (ok) toast("Audit export ready", format.toUpperCase());
  }
  return (
    <>
      <PageHeading
        eyebrow="Compliance"
        title="Audit log"
        text="Every authorized and denied mutation, written in the same transaction as the change."
      >
        <Button
          variant="secondary"
          icon="download"
          disabled={r.state !== "ready" || exporter.busy}
          onClick={() => exportLog("csv")}
        >
          Export CSV
        </Button>
        <Button
          variant="secondary"
          icon="download"
          disabled={r.state !== "ready" || exporter.busy}
          onClick={() => exportLog("ndjson")}
        >
          Export NDJSON
        </Button>
        <Button
          variant="secondary"
          icon="radio-tower"
          onClick={() => setSiem(true)}
        >
          Stream to SIEM
        </Button>
      </PageHeading>
      <Failure error={exporter.error} className="spaced" />
      {r.state === "unavailable" ? (
        <Unavailable what="The audit log (list and export)" />
      ) : (
        <>
          <Toolbar
            right={
              r.state === "ready"
                ? `${rows.length} event${rows.length === 1 ? "" : "s"} on this page`
                : undefined
            }
          >
            <Input
              icon="search"
              size="sm"
              aria-label="Principal ID"
              placeholder="Filter by principal ID"
              mono
              value={principal}
              onChange={val(reset(setPrincipal))}
              className="w200"
            />
            <Input
              size="sm"
              aria-label="Exact action"
              placeholder="Exact action, e.g. feed.create"
              mono
              value={action}
              onChange={val(reset(setAction))}
              className="w200"
            />
            <SegmentedControl
              aria-label="Result"
              options={[
                { value: "all", label: "All" },
                { value: "success", label: "Success" },
                { value: "denied", label: "Denied" },
                { value: "failure", label: "Failure" },
              ]}
              value={result}
              onChange={reset(setResult)}
            />
            <label className="row tight">
              <span className="muted small" aria-hidden="true">
                From
              </span>
              <Input
                size="sm"
                type="date"
                aria-label="From date"
                value={from}
                onChange={val(reset(setFrom))}
              />
            </label>
            <label className="row tight">
              <span className="muted small" aria-hidden="true">
                To
              </span>
              <Input
                size="sm"
                type="date"
                aria-label="To date"
                value={to}
                onChange={val(reset(setTo))}
              />
            </label>
          </Toolbar>
          {r.state === "loading" && <Loading what="audit events" />}
          {r.state === "error" && <Failure error={r.error} />}
          {r.state === "ready" &&
            (rows.length ? (
              <>
                <Table
                  columns={[
                    {
                      key: "created_at",
                      label: "Time",
                      render: (a: AuditEvent) => when(a.created_at),
                    },
                    {
                      key: "who",
                      label: "Principal",
                      render: (a: AuditEvent) => (
                        <span className="row tight">
                          <Icon
                            name={a.principal_id ? "user" : "globe"}
                            size={14}
                          />
                          <Mono>
                            {a.principal_id
                              ? (who.get(a.principal_id) ?? a.principal_id)
                              : a.action === "token.cleanup"
                                ? "system"
                                : "anonymous"}
                          </Mono>
                        </span>
                      ),
                    },
                    { key: "action", label: "Action", mono: true },
                    {
                      key: "resource_id",
                      label: "Resource",
                      render: (a: AuditEvent) => (
                        <span title={a.resource_id ?? undefined}>
                          {a.resource_id ? (
                            /^token\.(issue|revoke)$/.test(a.action) &&
                            knownTokens &&
                            !knownTokens.has(a.resource_id) ? (
                              <Mono muted>deleted token</Mono>
                            ) : /^pat\./.test(a.action) ? (
                              <Mono muted>
                                personal token {shortId(a.resource_id)}
                              </Mono>
                            ) : resourceNames.get(a.resource_id) ? (
                              <Mono>{resourceNames.get(a.resource_id)}</Mono>
                            ) : (
                              <Mono muted>{shortId(a.resource_id)}</Mono>
                            )
                          ) : (
                            "—"
                          )}
                        </span>
                      ),
                    },
                    {
                      key: "result",
                      label: "Result",
                      render: (a: AuditEvent) => (
                        <Badge tone={tone(a.result)} dot>
                          {a.result}
                        </Badge>
                      ),
                    },
                  ]}
                  rows={rows}
                />
                <Pager
                  cursor={cursor}
                  next={r.data.page.next_cursor}
                  set={setCursor}
                />
              </>
            ) : (
              <EmptyState
                icon="scroll-text"
                title="No events match"
                description="No audit records match these filters."
              />
            ))}
        </>
      )}
      {siem && <Siem client={client} onClose={() => setSiem(false)} />}
    </>
  );
}

function Siem({ client, onClose }: { client: Client; onClose: () => void }) {
  const r = useOptionalCall(client, "getAuditSink");
  return (
    <Modal
      title="Stream audit events"
      description="Forward audit records to a SIEM through a syslog or HTTP sink."
      onClose={onClose}
      width={600}
      actions={<Button onClick={onClose}>Done</Button>}
    >
      {r.state === "loading" && <Loading what="audit sink" />}
      {r.state === "unavailable" && <Unavailable what="Audit streaming" />}
      {r.state === "error" && <Failure error={r.error} />}
      {r.state === "ready" && (
        <SinkForm
          key={r.data.revision}
          client={client}
          sink={r.data}
          reload={r.reload}
        />
      )}
    </Modal>
  );
}
function SinkForm({
  client,
  sink,
  reload,
}: {
  client: Client;
  sink: AuditSink;
  reload: () => void;
}) {
  const [kind, setKind] = useState<"syslog" | "http">(sink.kind ?? "http");
  const [endpoint, setEndpoint] = useState(sink.endpoint ?? "");
  const [credential, setCredential] = useState("");
  const [enabled, setEnabled] = useState(sink.enabled || !sink.configured);
  const action = useAction();
  const toast = useToast();
  const etag = () => requireETag(`"${sink.revision}"`);
  return (
    <form
      className="stack"
      onSubmit={async (e) => {
        e.preventDefault();
        const submitted = credential;
        setCredential("");
        const ok = await action.run(() =>
          client.call("putAuditSink", {
            etag: etag(),
            body: {
              kind,
              endpoint,
              enabled,
              ...(submitted ? { credential: submitted } : {}),
            },
          }),
        );
        if (ok) {
          toast("Audit sink saved", endpoint);
          reload();
        }
      }}
    >
      {sink.configured && (
        <PropertyList
          items={[
            {
              label: "Delivery",
              value: (
                <Badge tone={sink.last_error ? "bad" : "good"} dot>
                  {sink.last_error
                    ? "Failing"
                    : sink.enabled
                      ? "Streaming"
                      : "Paused"}
                </Badge>
              ),
            },
            { label: "Checkpoint", value: sink.checkpoint, mono: true },
            { label: "Last forwarded", value: when(sink.last_forwarded_at) },
            ...(sink.last_error
              ? [{ label: "Last error", value: sink.last_error }]
              : []),
          ]}
        />
      )}
      <Select
        label="Sink kind"
        options={[
          { value: "http", label: "HTTP" },
          { value: "syslog", label: "Syslog" },
        ]}
        value={kind}
        onChange={val((v) => setKind(v as "syslog" | "http"))}
      />
      <Input
        label="Endpoint"
        mono
        required
        maxLength={2048}
        value={endpoint}
        onChange={val(setEndpoint)}
      />
      <Input
        label="Credential"
        type="password"
        mono
        autoComplete="new-password"
        value={credential}
        onChange={val(setCredential)}
        hint={
          sink.has_credential
            ? "A credential is stored. Leave empty to keep it; it is never shown."
            : "Stored encrypted and never shown again."
        }
      />
      <Switch
        label="Enabled"
        labelPosition="end"
        checked={enabled}
        onChange={chk(setEnabled)}
      />
      <Failure error={action.error} />
      <div className="row">
        <Button type="submit" disabled={action.busy}>
          {action.busy ? "Saving…" : "Save audit sink"}
        </Button>
        {sink.configured && (
          <Button
            type="button"
            variant="danger"
            disabled={action.busy}
            onClick={async () => {
              const ok = await action.run(() =>
                client.call("deleteAuditSink", { etag: etag() }),
              );
              if (ok) {
                toast("Audit sink removed");
                reload();
              }
            }}
          >
            Remove sink
          </Button>
        )}
      </div>
    </form>
  );
}
