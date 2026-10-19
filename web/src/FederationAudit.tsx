import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type { FederationAuditEvent, FederationAuditMember } from "./generated";
import { Alert, Badge, EmptyState, Input, Select, Table } from "./ds/bundle";
import {
  Failure,
  Loading,
  Mono,
  Pager,
  SectionHead,
  Toolbar,
  useDebounced,
  when,
} from "./ui";

const statusTone: Record<string, "good" | "warn" | "bad"> = {
  ok: "good",
  forbidden: "warn",
  timeout: "warn",
  skipped: "warn",
  error: "bad",
};

// Federated audit: every member's audit stream, newest first. Each member
// authorizes the caller itself (instance admin there), so a member that shows
// nothing may simply deny; the per-member status makes that explicit.
export function FederationAudit({ client }: { client: Client }) {
  const [prefix, setPrefix] = useState("federation.");
  const [result, setResult] = useState("");
  const [cursor, setCursor] = useState("");
  const dp = useDebounced(prefix);
  const query: Record<string, string> = {
    limit: "50",
    ...(dp ? { prefix: dp } : {}),
    ...(result ? { result } : {}),
    ...(cursor ? { cursor } : {}),
  };
  const audit = useOptionalCall(client, "federatedAudit", { query });
  if (audit.state === "unavailable") return null;
  const rows = audit.state === "ready" ? audit.data.items : [];
  return (
    <>
      <SectionHead title="Federated audit" />
      <Toolbar
        right={audit.state === "ready" ? <>{rows.length} events</> : undefined}
      >
        <Input
          icon="search"
          size="sm"
          aria-label="Action prefix"
          placeholder="Action prefix"
          value={prefix}
          onChange={(e: { target: { value: string } }) => {
            setPrefix(e.target.value);
            setCursor("");
          }}
          className="w300"
        />
        <Select
          size="sm"
          aria-label="Result"
          options={[
            { value: "", label: "Any result" },
            { value: "success", label: "success" },
            { value: "denied", label: "denied" },
            { value: "failure", label: "failure" },
          ]}
          value={result}
          onChange={(e: { target: { value: string } }) => {
            setResult(e.target.value);
            setCursor("");
          }}
        />
      </Toolbar>
      {audit.state === "loading" && <Loading what="federated audit" />}
      {audit.state === "error" && <Failure error={audit.error} />}
      {audit.state === "ready" && (
        <>
          {audit.data.partial && (
            <Alert tone="warn" title="Partial results">
              Some members did not answer, or do not let you read their audit
              log (it needs instance administration on that member). Nothing is
              hidden silently: see the member status.
            </Alert>
          )}
          <MemberStatus members={audit.data.members} />
          {rows.length ? (
            <>
              <Table
                columns={[
                  {
                    key: "created_at",
                    label: "When",
                    render: (e: FederationAuditEvent) => when(e.created_at),
                  },
                  { key: "member_name", label: "Member" },
                  { key: "action", label: "Action", mono: true },
                  {
                    key: "result",
                    label: "Result",
                    render: (e: FederationAuditEvent) => (
                      <Badge
                        tone={
                          e.result === "success"
                            ? "good"
                            : e.result === "denied"
                              ? "warn"
                              : "bad"
                        }
                      >
                        {e.result}
                      </Badge>
                    ),
                  },
                  {
                    key: "details",
                    label: "Details",
                    render: (e: FederationAuditEvent) => (
                      <Mono muted>{JSON.stringify(e.details ?? {})}</Mono>
                    ),
                  },
                ]}
                rows={rows}
              />
              <Pager
                cursor={cursor}
                next={audit.data.page.next_cursor}
                set={setCursor}
              />
            </>
          ) : (
            <EmptyState
              icon="search-x"
              title="No events"
              description="No member shows you matching audit events."
            />
          )}
        </>
      )}
    </>
  );
}

function MemberStatus({ members }: { members: FederationAuditMember[] }) {
  return (
    <div className="row" aria-label="Member status">
      {members.map((m) => (
        <Badge key={m.id} tone={statusTone[m.status] ?? "neutral"}>
          {m.name}: {m.status}
          {m.code ? ` (${m.code})` : ""} · {m.events} · {m.took_ms} ms
        </Badge>
      ))}
    </div>
  );
}
