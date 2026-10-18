import { useState } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type { FederationSearchHit, FederationSearchMember } from "./generated";
import { Alert, Badge, Button, EmptyState, Input, Table } from "./ds/bundle";
import { Failure, Loading, Mono, Pager, Toolbar, useDebounced } from "./ui";

const statusTone: Record<string, "good" | "warn" | "bad"> = {
  ok: "good",
  timeout: "warn",
  skipped: "warn",
  error: "bad",
};

// Cross-instance search. Every member applies its own authorization to the
// caller, so a member that shows nothing may simply allow nothing; the per
// member status makes slow, unreachable or refusing members explicit.
export function FederatedSearch({ client }: { client: Client }) {
  const [q, setQ] = useState("");
  const [digest, setDigest] = useState("");
  const [cursor, setCursor] = useState("");
  const dq = useDebounced(q);
  const dd = useDebounced(digest);
  const digestOK = !dd || /^sha256:[0-9a-f]{64}$/.test(dd);
  const query: Record<string, string> = {
    limit: "50",
    ...(dq ? { q: dq } : {}),
    ...(dd && digestOK ? { digest: dd } : {}),
    ...(cursor ? { cursor } : {}),
  };
  const search = useOptionalCall(client, "federatedSearch", { query });
  const rows = search.state === "ready" ? search.data.items : [];
  return (
    <>
      <Toolbar
        right={
          search.state === "ready" ? <>{rows.length} packages</> : undefined
        }
      >
        <Input
          icon="search"
          size="sm"
          aria-label="Search the federation"
          placeholder="Package name"
          value={q}
          onChange={(e: { target: { value: string } }) => {
            setQ(e.target.value);
            setCursor("");
          }}
          className="w300"
        />
        <Input
          size="sm"
          mono
          aria-label="SHA-256 checksum"
          placeholder="sha256:…"
          error={digestOK ? undefined : "sha256: followed by 64 hex digits"}
          value={digest}
          onChange={(e: { target: { value: string } }) => {
            setDigest(e.target.value);
            setCursor("");
          }}
        />
      </Toolbar>
      {search.state === "loading" && <Loading what="federated packages" />}
      {search.state === "unavailable" && (
        <Alert tone="info" title="Federated search is not available">
          This server does not provide cross-instance search.
        </Alert>
      )}
      {search.state === "error" && <Failure error={search.error} />}
      {search.state === "ready" && (
        <>
          {search.data.partial && (
            <Alert tone="warn" title="Partial results">
              Some members did not answer in time or refused the query. Results
              from the others are shown; nothing is hidden silently.
            </Alert>
          )}
          <MemberStatus members={search.data.members} />
          {rows.length ? (
            <>
              <Table
                columns={[
                  {
                    key: "coordinate",
                    label: "Package",
                    render: (r: FederationSearchHit) => (
                      <>
                        {r.coordinate}
                        <small>{r.namespace || "(root namespace)"}</small>
                      </>
                    ),
                  },
                  {
                    key: "feed_key",
                    label: "Feed",
                    mono: true,
                  },
                  {
                    key: "format",
                    label: "Format",
                    render: (r: FederationSearchHit) => (
                      <Badge tone="outline">{r.format}</Badge>
                    ),
                  },
                  {
                    key: "member_name",
                    label: "Held by",
                    render: (r: FederationSearchHit) => (
                      <span className="row tight inline">
                        {r.copies.map((c) => (
                          <Badge
                            key={c.member}
                            tone={c.home ? "pillar" : "neutral"}
                          >
                            {c.member_name}
                            {c.home ? "" : " (replica)"}
                          </Badge>
                        ))}
                      </span>
                    ),
                  },
                ]}
                rows={rows}
              />
              <Pager
                cursor={cursor}
                next={search.data.page.next_cursor}
                set={setCursor}
              />
            </>
          ) : (
            <EmptyState
              icon="search-x"
              title="No packages match"
              description="No member shows you a matching package. Each member applies its own access rules."
              action={
                <Button
                  size="sm"
                  variant="secondary"
                  onClick={() => {
                    setQ("");
                    setDigest("");
                    setCursor("");
                  }}
                >
                  Clear filters
                </Button>
              }
            />
          )}
        </>
      )}
    </>
  );
}

function MemberStatus({ members }: { members: FederationSearchMember[] }) {
  return (
    <div className="row" aria-label="Member status">
      {members.map((m) => (
        <Badge key={m.id} tone={statusTone[m.status] ?? "neutral"}>
          {m.name}: {m.status}
          {m.code ? ` (${m.code})` : ""} · {m.hits} · {m.took_ms} ms
        </Badge>
      ))}
      <Mono muted>{members.length} members</Mono>
    </div>
  );
}
