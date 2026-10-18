import { useState } from "react";
import type { Client } from "./api";
import { paths, useOptional, type AuditRecord } from "./gaps";
import {
  Badge,
  Button,
  EmptyState,
  Icon,
  Input,
  Modal,
  SegmentedControl,
  Select,
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
  useDebounced,
  val,
  when,
} from "./ui";

const actions = [
  ["", "All actions"],
  ["artifact.", "artifact.*"],
  ["feed.", "feed.*"],
  ["source.", "source.*"],
  ["identity.", "identity.*"],
  ["token.", "token.*"],
] as const;

function tone(result?: string) {
  const r = (result ?? "").toLowerCase();
  if (r.startsWith("denied") || r === "error") return "bad";
  if (r.startsWith("conflict")) return "warn";
  return "good";
}
function csvCell(v: unknown) {
  const s = String(v ?? "");
  // Guard spreadsheet formula injection as well as quoting.
  const safe = /^[=+\-@\t\r]/.test(s) ? "'" + s : s;
  return '"' + safe.replaceAll('"', '""') + '"';
}

export function AuditPage({ client }: { client: Client }) {
  const [q, setQ] = useState("");
  const [action, setAction] = useState("");
  const [result, setResult] = useState("all");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const [after, setAfter] = useState("");
  const [siem, setSiem] = useState(false);
  const dq = useDebounced(q);
  const r = useOptional<{ items: AuditRecord[]; next_after?: string }>(
    client,
    paths.audit,
    {
      limit: "50",
      ...(dq ? { principal: dq } : {}),
      ...(action ? { action } : {}),
      ...(result !== "all" ? { result } : {}),
      ...(from ? { from: new Date(from).toISOString() } : {}),
      ...(to ? { to: new Date(to + "T23:59:59").toISOString() } : {}),
      ...(after ? { after } : {}),
    },
  );
  const rows = r.state === "ready" ? r.data.items : [];
  function exportCSV() {
    const head = ["time", "principal", "action", "resource", "result"];
    const body = rows.map((a) => [
      a.at,
      a.principal_subject ?? a.principal_id,
      a.action,
      a.resource,
      a.result,
    ]);
    const csv = [head, ...body]
      .map((l) => l.map(csvCell).join(","))
      .join("\r\n");
    const url = URL.createObjectURL(new Blob([csv], { type: "text/csv" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = "audit-log.csv";
    a.click();
    URL.revokeObjectURL(url);
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
          disabled={!rows.length}
          onClick={exportCSV}
        >
          Export CSV
        </Button>
        <Button
          variant="secondary"
          icon="radio-tower"
          onClick={() => setSiem(true)}
        >
          Stream to SIEM
        </Button>
      </PageHeading>
      {r.state === "unavailable" ? (
        <>
          <Unavailable what="The audit log (list and export)" />
          <Needs block>audit list + export</Needs>
        </>
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
              aria-label="Principal"
              placeholder="Principal"
              value={q}
              onChange={(e: { target: { value: string } }) => {
                setQ(e.target.value);
                setAfter("");
              }}
              className="w260"
            />
            <Select
              size="sm"
              aria-label="Action"
              options={actions.map(([value, label]) => ({ value, label }))}
              value={action}
              onChange={(e: { target: { value: string } }) => {
                setAction(e.target.value);
                setAfter("");
              }}
            />
            <SegmentedControl
              aria-label="Result"
              options={[
                { value: "all", label: "All" },
                { value: "allowed", label: "Allowed" },
                { value: "denied", label: "Denied" },
              ]}
              value={result}
              onChange={(v: string) => {
                setResult(v);
                setAfter("");
              }}
            />
            <Input
              size="sm"
              type="date"
              aria-label="From date"
              value={from}
              onChange={val((v) => {
                setFrom(v);
                setAfter("");
              })}
            />
            <Input
              size="sm"
              type="date"
              aria-label="To date"
              value={to}
              onChange={val((v) => {
                setTo(v);
                setAfter("");
              })}
            />
          </Toolbar>
          {r.state === "loading" && <Loading what="audit events" />}
          {r.state === "error" && <Failure error={r.error} />}
          {r.state === "ready" &&
            (rows.length ? (
              <>
                <Table
                  rowKey="id"
                  columns={[
                    {
                      key: "at",
                      label: "Time",
                      mono: true,
                      render: (a: AuditRecord) => when(a.at),
                    },
                    {
                      key: "who",
                      label: "Principal",
                      render: (a: AuditRecord) => (
                        <span className="row tight">
                          <Icon
                            name={
                              a.principal_kind === "service"
                                ? "bot"
                                : a.principal_kind === "anonymous"
                                  ? "globe"
                                  : "user"
                            }
                            size={14}
                          />
                          <Mono>
                            {a.principal_subject ??
                              a.principal_id ??
                              "anonymous"}
                          </Mono>
                        </span>
                      ),
                    },
                    { key: "action", label: "Action", mono: true },
                    { key: "resource", label: "Resource" },
                    {
                      key: "result",
                      label: "Result",
                      render: (a: AuditRecord) => (
                        <Badge tone={tone(a.result)} dot>
                          {a.result ?? "—"}
                        </Badge>
                      ),
                    },
                  ]}
                  rows={rows.map((a, i) => ({ ...a, id: a.id ?? i }))}
                />
                <Pager cursor={after} next={r.data.next_after} set={setAfter} />
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
