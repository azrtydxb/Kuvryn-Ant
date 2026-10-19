import { useState, type ReactNode } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type {
  MigrationCompareReport,
  MigrationCompareResult,
  MigrationReport,
  TransferDocument,
  TransferItem,
  TransferReport,
} from "./generated";
import {
  Alert,
  Badge,
  Button,
  Input,
  Switch,
  Table,
  Tabs,
  Textarea,
} from "./ds/bundle";
import {
  Card,
  Digest,
  Failure,
  Loading,
  Mono,
  PageHeading,
  Unavailable,
  chk,
  count,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

const list = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);

const TONE: Record<string, "good" | "info" | "neutral" | "warn" | "bad"> = {
  created: "good",
  updated: "info",
  unchanged: "neutral",
  skipped: "warn",
  conflict: "bad",
};

/** Inventory and mapping report of an import or migration (dry run or applied). */
export function ReportView({ report }: { report: TransferReport }) {
  const kinds = Object.entries(report.summary);
  return (
    <div className="stack" data-testid="transfer-report">
      <Alert
        tone={
          report.integrity_failures.length || report.summary.conflict
            ? "warn"
            : "info"
        }
        title={report.dry_run ? "Dry run — nothing was changed" : "Applied"}
      >
        {report.dry_run
          ? "This is exactly what applying would do."
          : "The changes below were written."}
      </Alert>
      {kinds.length > 0 && (
        <Table
          columns={[
            { key: "kind", label: "Object" },
            ...["created", "updated", "unchanged", "skipped", "conflict"].map(
              (a) => ({
                key: a,
                label: a[0].toUpperCase() + a.slice(1),
                align: "right" as const,
                render: (r: { kind: string; counts: Record<string, number> }) =>
                  count(r.counts[a] ?? 0),
              }),
            ),
          ]}
          rows={kinds.map(([kind, counts]) => ({ kind, counts }))}
        />
      )}
      {report.items.length > 0 && (
        <>
          <h3>Changes and problems</h3>
          <Table
            columns={[
              { key: "kind", label: "Object" },
              {
                key: "name",
                label: "Name",
                render: (i: TransferItem) => <Mono>{i.name}</Mono>,
              },
              {
                key: "action",
                label: "Action",
                render: (i: TransferItem) => (
                  <Badge tone={TONE[i.action]} dot>
                    {i.action}
                  </Badge>
                ),
              },
              { key: "reason", label: "Detail" },
            ]}
            rows={report.items}
          />
          {report.items_truncated && (
            <p className="muted">List truncated; counts above are complete.</p>
          )}
        </>
      )}
      {report.unmapped.length > 0 && (
        <>
          <h3>Needs attention (not mapped automatically)</h3>
          <Table
            columns={[
              { key: "kind", label: "Kind" },
              { key: "name", label: "Name" },
              { key: "reason", label: "Why" },
            ]}
            rows={report.unmapped}
          />
        </>
      )}
      {report.integrity_failures.length > 0 && (
        <>
          <h3>Integrity failures</h3>
          <Table
            columns={[
              {
                key: "digest",
                label: "Digest",
                render: (f: { digest: string }) => <Digest value={f.digest} />,
              },
              { key: "reason", label: "Reason" },
            ]}
            rows={report.integrity_failures}
          />
        </>
      )}
    </div>
  );
}

function download(name: string, data: unknown) {
  const url = URL.createObjectURL(
    new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }),
  );
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
}

function ExportCard({
  client,
  onDocument,
}: {
  client: Client;
  onDocument: (d: TransferDocument) => void;
}) {
  const [feeds, setFeeds] = useState("");
  const [namespaces, setNamespaces] = useState("");
  const [content, setContent] = useState(false);
  const action = useAction();
  const toast = useToast();
  return (
    <Card title="Export">
      <p className="muted">
        Exports feed and source definitions, policies and remote configuration
        as a portable document. Credential values are never exported. Use{" "}
        <Mono>ant export</Mono> on the server for a verifiable bundle that also
        contains artifact bytes.
      </p>
      <div className="cols">
        <Input
          label="Feeds"
          hint="Space or comma separated slugs. Empty exports all feeds."
          value={feeds}
          onChange={val(setFeeds)}
        />
        <Input
          label="Namespaces"
          hint="Limits the content inventory."
          value={namespaces}
          onChange={val(setNamespaces)}
        />
      </div>
      <Switch
        label="Include content inventory (digests and metadata)"
        labelPosition="end"
        checked={content}
        onChange={chk(setContent)}
      />
      <Failure error={action.error} />
      <Button
        icon="download"
        disabled={action.busy}
        onClick={async () => {
          let doc: TransferDocument | undefined;
          const ok = await action.run(async () => {
            doc = (
              await client.call("exportTransfer", {
                body: {
                  feeds: list(feeds),
                  namespaces: list(namespaces),
                  include_content: content,
                },
              })
            ).data;
          });
          if (ok && doc) {
            onDocument(doc);
            download("ant-export.json", doc);
            toast(
              "Export ready",
              `${doc.feeds.length} feeds · ${doc.sources.length} sources`,
            );
          }
        }}
      >
        {action.busy ? "Exporting…" : "Export definitions"}
      </Button>
    </Card>
  );
}

function ImportCard({
  client,
  text,
  setText,
  reloadRuns,
}: {
  client: Client;
  text: string;
  setText: (s: string) => void;
  reloadRuns: () => void;
}) {
  const [report, setReport] = useState<TransferReport>();
  const [dryDone, setDryDone] = useState("");
  const action = useAction();
  const toast = useToast();
  const [parseError, setParseError] = useState("");
  const run = async (dry: boolean) => {
    let doc: TransferDocument;
    try {
      doc = JSON.parse(text);
      setParseError("");
    } catch {
      setParseError("The document is not valid JSON.");
      return;
    }
    const ok = await action.run(async () => {
      const r = (
        await client.call("importTransfer", {
          body: { document: doc, dry_run: dry },
        })
      ).data;
      setReport(r);
      if (dry) setDryDone(text);
    });
    if (ok) {
      toast(dry ? "Dry run complete" : "Import applied");
      reloadRuns();
    }
  };
  return (
    <Card title="Import">
      <p className="muted">
        Paste or load an export document. A dry run reports what would be
        created, updated, skipped or conflict, without changing anything. Apply
        is idempotent and never overwrites existing content. Artifact bytes come
        from <Mono>ant import</Mono> on the server.
      </p>
      <input
        type="file"
        accept="application/json,.json"
        aria-label="Load export file"
        onChange={async (e) => {
          const f = e.target.files?.[0];
          if (f && f.size <= 64 << 20) setText(await f.text());
        }}
      />
      <Textarea
        label="Export document"
        mono
        rows={8}
        value={text}
        onChange={val(setText)}
      />
      {parseError && <Alert tone="bad">{parseError}</Alert>}
      <Failure error={action.error} />
      <div className="row">
        <Button
          variant="secondary"
          disabled={action.busy || !text.trim()}
          onClick={() => run(true)}
        >
          Dry run
        </Button>
        <Button
          disabled={action.busy || !text.trim() || dryDone !== text}
          onClick={() => run(false)}
          title={dryDone !== text ? "Run a dry run of this document first" : ""}
        >
          Apply import
        </Button>
      </div>
      {report && <ReportView report={report} />}
    </Card>
  );
}

function MigrateCard({
  client,
  reloadRuns,
}: {
  client: Client;
  reloadRuns: () => void;
}) {
  const [url, setUrl] = useState("");
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [repos, setRepos] = useState("");
  const [counts, setCounts] = useState(true);
  const [report, setReport] = useState<MigrationReport>();
  const [coords, setCoords] = useState("");
  const [compare, setCompare] = useState<MigrationCompareReport>();
  const action = useAction();
  const cmp = useAction();
  const toast = useToast();
  const creds = {
    url,
    ...(user ? { username: user } : {}),
    ...(password ? { password } : {}),
    ...(token ? { token } : {}),
  };
  const items = coords
    .split("\n")
    .map((l) => l.trim().split(/\s+/))
    .filter((p) => p.length >= 2 && !p[0].startsWith("#"))
    .map(([repository, path, feed]) => ({
      repository,
      path,
      ...(feed ? { feed } : {}),
    }));
  const migrate = async (dry: boolean) => {
    const ok = await action.run(async () => {
      setReport(
        (
          await client.call("migrateNexus", {
            body: {
              ...creds,
              repositories: list(repos),
              count_assets: counts,
              dry_run: dry,
            },
          })
        ).data,
      );
    });
    if (ok) {
      toast(dry ? "Migration dry run complete" : "Definitions migrated");
      reloadRuns();
    }
  };
  return (
    <Card title="Migrate from Nexus 3">
      <p className="muted">
        Reads the repository inventory through the Nexus REST API: hosted
        becomes a local source, proxy a remote source, a group a feed with
        ordered sources, and cleanup policies become retention. Credentials are
        used for the request only and are never stored. Copy content with{" "}
        <Mono>ant migrate nexus --copy-content --apply</Mono>.
      </p>
      <Input
        label="Nexus URL"
        placeholder="https://nexus.example.com"
        value={url}
        onChange={val(setUrl)}
      />
      <div className="cols">
        <Input
          label="Username"
          autoComplete="off"
          value={user}
          onChange={val(setUser)}
        />
        <Input
          label="Password"
          type="password"
          autoComplete="off"
          value={password}
          onChange={val(setPassword)}
        />
        <Input
          label="Or bearer token"
          type="password"
          autoComplete="off"
          value={token}
          onChange={val(setToken)}
        />
      </div>
      <Input
        label="Repositories"
        hint="Space or comma separated. Empty migrates every repository."
        value={repos}
        onChange={val(setRepos)}
      />
      <Switch
        label="Count assets and bytes per hosted repository"
        labelPosition="end"
        checked={counts}
        onChange={chk(setCounts)}
      />
      <Failure error={action.error} />
      <div className="row">
        <Button
          variant="secondary"
          disabled={action.busy || !url}
          onClick={() => migrate(true)}
        >
          {action.busy ? "Reading…" : "Dry run"}
        </Button>
        <Button
          disabled={action.busy || !url || !report?.dry_run}
          title={!report?.dry_run ? "Run a dry run first" : ""}
          onClick={() => migrate(false)}
        >
          Apply definitions
        </Button>
      </div>
      {report && (
        <>
          <h3>Repositories</h3>
          <Table
            columns={[
              { key: "name", label: "Repository" },
              { key: "type", label: "Type" },
              { key: "format", label: "Format" },
              { key: "mapped_to", label: "Maps to" },
              {
                key: "assets",
                label: "Assets",
                align: "right",
                render: (r: { assets?: number }) => count(r.assets),
              },
            ]}
            rows={report.repositories}
          />
          <ReportView report={report} />
        </>
      )}
      <h3>Compare during staged adoption</h3>
      <p className="muted">
        For each coordinate, compare what a client receives from Nexus with what
        it would receive from the Ant feed. One per line:{" "}
        <Mono>repository path [feed]</Mono>.
      </p>
      <Textarea
        label="Coordinates"
        mono
        rows={4}
        value={coords}
        onChange={val(setCoords)}
      />
      <Failure error={cmp.error} />
      <Button
        variant="secondary"
        disabled={cmp.busy || !url || !items.length}
        onClick={async () => {
          const ok = await cmp.run(async () => {
            setCompare(
              (
                await client.call("compareMigration", {
                  body: { ...creds, items },
                })
              ).data,
            );
          });
          if (ok) {
            toast("Comparison complete");
            reloadRuns();
          }
        }}
      >
        {cmp.busy ? "Comparing…" : "Compare"}
      </Button>
      {compare && (
        <Table
          columns={[
            {
              key: "path",
              label: "Coordinate",
              render: (r: MigrationCompareResult) => (
                <Mono>{`${r.repository}/${r.path}`}</Mono>
              ),
            },
            {
              key: "status",
              label: "Result",
              render: (r: MigrationCompareResult) => (
                <Badge
                  tone={
                    r.status === "identical"
                      ? "good"
                      : r.status === "error"
                        ? "bad"
                        : "warn"
                  }
                  dot
                >
                  {r.status.replaceAll("_", " ")}
                </Badge>
              ),
            },
            {
              key: "source",
              label: "Nexus",
              render: (r: MigrationCompareResult) => (
                <Mono muted>{r.source.sha256?.slice(0, 12) ?? "—"}</Mono>
              ),
            },
            {
              key: "ant",
              label: "Ant",
              render: (r: MigrationCompareResult) => (
                <Mono muted>{r.ant.sha256?.slice(0, 12) ?? "—"}</Mono>
              ),
            },
            {
              key: "detail",
              label: "Detail",
              render: (r: MigrationCompareResult) =>
                r.ant.detail || r.source.detail || "",
            },
          ]}
          rows={compare.results}
        />
      )}
    </Card>
  );
}

export function MigrationPage({ client }: { client: Client }) {
  const runs = useOptionalCall(client, "listTransferRuns");
  const [text, setText] = useState("");
  const [tab, setTab] = useState("export");
  const runList = runs.state === "ready" ? runs.data.items : [];
  // Panels stay mounted (hidden) so dry-run reports and typed input survive tab switches.
  const panel = (id: string, children: ReactNode) => (
    <div className="stack" hidden={tab !== id} role="tabpanel">
      {children}
    </div>
  );
  return (
    <>
      <PageHeading
        eyebrow="Import & migrate"
        title="Import, export and migration"
        text="Move definitions between instances, or adopt Ant from another artifact manager and compare results before switching clients."
      />
      <Tabs
        value={tab}
        onChange={setTab}
        className="spaced-tabs"
        items={[
          { id: "export", label: "Export" },
          { id: "import", label: "Import" },
          { id: "nexus", label: "Migrate from Nexus" },
          { id: "history", label: "History", count: runList.length },
        ]}
      />
      {panel(
        "export",
        <ExportCard
          client={client}
          onDocument={(d) => {
            setText(JSON.stringify(d, null, 2));
          }}
        />,
      )}
      {panel(
        "import",
        <ImportCard
          client={client}
          text={text}
          setText={setText}
          reloadRuns={runs.reload}
        />,
      )}
      {panel("nexus", <MigrateCard client={client} reloadRuns={runs.reload} />)}
      {panel(
        "history",
        <Card title="History">
          {runs.state === "loading" && <Loading what="history" />}
          {runs.state === "unavailable" && (
            <Unavailable what="Transfer history" />
          )}
          {runs.state === "error" && <Failure error={runs.error} />}
          {runs.state === "ready" &&
            (runs.data.items.length ? (
              <Table
                columns={[
                  {
                    key: "created_at",
                    label: "When",
                    render: (r: { created_at: string }) => when(r.created_at),
                  },
                  { key: "kind", label: "Kind" },
                  {
                    key: "dry_run",
                    label: "Mode",
                    render: (r: { dry_run: boolean }) =>
                      r.dry_run ? "Dry run / read only" : "Applied",
                  },
                  {
                    key: "summary",
                    label: "Summary",
                    render: (r: { summary: object }) => (
                      <Mono muted>{JSON.stringify(r.summary)}</Mono>
                    ),
                  },
                ]}
                rows={runs.data.items}
              />
            ) : (
              <p className="muted">No transfers yet.</p>
            ))}
        </Card>,
      )}
    </>
  );
}
