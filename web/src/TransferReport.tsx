import type { TransferItem, TransferReport } from "./generated";
import { Alert, Badge, Table } from "./ds/bundle";
import { Digest, Mono, count } from "./ui";

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
