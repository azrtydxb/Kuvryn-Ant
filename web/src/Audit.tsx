import { useMemo, useState } from "react";
import type { Client } from "./api";
import { useResource } from "./ui";
import { useOptionalCall, paths, useOptional } from "./gaps";
import type { AuditEvent } from "./generated";
import {
  Badge,
  Button,
  EmptyState,
  Icon,
  Input,
  Modal,
  SegmentedControl,
  Table,
} from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Needs,
  PageHeading,
  Pager,
  Toolbar,
  Unavailable,
  useAction,
  useDebounced,
  useToast,
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
              placeholder="Principal ID"
              mono
              value={principal}
              onChange={val(reset(setPrincipal))}
              className="w260"
            />
            <Input
              size="sm"
              aria-label="Exact action"
              placeholder="Exact action, e.g. feed.create"
              mono
              value={action}
              onChange={val(reset(setAction))}
              className="w260"
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
            <Input
              size="sm"
              type="date"
              aria-label="From date"
              value={from}
              onChange={val(reset(setFrom))}
            />
            <Input
              size="sm"
              type="date"
              aria-label="To date"
              value={to}
              onChange={val(reset(setTo))}
            />
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
                      mono: true,
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
                        <Mono muted>{a.resource_id ?? "—"}</Mono>
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
  const r = useOptional<{
    items?: { id?: string; kind?: string; target?: string }[];
  }>(client, paths.auditSinks);
  return (
    <Modal
      title="Stream audit events"
      description="Forward audit records to a SIEM through a syslog or HTTP sink."
      onClose={onClose}
      actions={<Button onClick={onClose}>Done</Button>}
    >
      {r.state === "loading" && <Loading what="audit sinks" />}
      {r.state === "unavailable" && <Unavailable what="Audit streaming" />}
      {r.state === "error" && <Failure error={r.error} />}
      {r.state === "ready" && (
        <>
          {(r.data.items ?? []).length ? (
            <ul className="plainlist">
              {r.data.items!.map((s, i) => (
                <li key={s.id ?? i}>
                  <Mono>{s.kind}</Mono> {s.target}
                </li>
              ))}
            </ul>
          ) : (
            <p className="muted">No audit sinks are configured.</p>
          )}
          <p className="muted small">
            Creating sinks from the console is not available yet.
          </p>
        </>
      )}
      <Needs block>audit streaming to a SIEM (syslog/HTTP sink)</Needs>
    </Modal>
  );
}
