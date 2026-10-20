import { useState, type ReactNode } from "react";
import type { Client } from "./api";
import { useOptionalCall } from "./gaps";
import type { TransferDocument, TransferReport } from "./generated";
import {
  Alert,
  Button,
  Input,
  Switch,
  Table,
  Tabs,
  Textarea,
} from "./ds/bundle";
import { MigratePanel } from "./MigrateSource";
import { ReportView } from "./TransferReport";
import {
  Card,
  Failure,
  Loading,
  Mono,
  PageHeading,
  Unavailable,
  chk,
  useAction,
  useToast,
  val,
  when,
} from "./ui";

export { ReportView };

const list = (s: string) =>
  s
    .split(/[\s,]+/)
    .map((x) => x.trim())
    .filter(Boolean);

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
    <div className={tab === "migrate" ? "mig mig--fit" : "mig"}>
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
          { id: "migrate", label: "Migrate" },
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
      {panel(
        "migrate",
        <MigratePanel client={client} reloadRuns={runs.reload} />,
      )}
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
    </div>
  );
}
