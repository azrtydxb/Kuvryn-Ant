import { useMemo, useState } from "react";
import type { Client } from "./api";
import type {
  MigrationCompareReport,
  MigrationCompareResult,
  MigrationReport,
  MigrationRepository,
} from "./generated";
import {
  Badge,
  Button,
  Checkbox,
  Drawer,
  EmptyState,
  Input,
  Modal,
  SegmentedControl,
  Switch,
  Table,
  Textarea,
} from "./ds/bundle";
import { ReportView } from "./TransferReport";
import {
  Failure,
  Mono,
  bytes,
  chk,
  count,
  useAction,
  useToast,
  val,
} from "./ui";

export type SourceKind = "nexus" | "artifactory";

type Spec = {
  label: string;
  urlLabel: string;
  placeholder: string;
  urlHint: string;
  /** The source's own names for the neutral hosted/proxy/group types. */
  types: Record<string, string>;
  secretLabel: string;
  tokenLabel: string;
  defaultAuth: "basic" | "token";
  /** Whether sizes come from the manager's own summary (no asset listing needed). */
  countsByDefault: boolean;
  cli: string;
};

const SPECS: Record<SourceKind, Spec> = {
  nexus: {
    label: "Nexus",
    urlLabel: "Nexus URL",
    placeholder: "https://nexus.example.com",
    urlHint: "Base URL of Nexus Repository 3.",
    types: { hosted: "hosted", proxy: "proxy", group: "group" },
    secretLabel: "Password",
    tokenLabel: "Bearer token",
    defaultAuth: "basic",
    countsByDefault: true,
    cli: "ant migrate nexus",
  },
  artifactory: {
    label: "Artifactory",
    urlLabel: "Artifactory URL",
    placeholder: "https://example.jfrog.io/artifactory",
    urlHint: "/artifactory is added when the URL has no path.",
    types: { hosted: "local", proxy: "remote", group: "virtual" },
    secretLabel: "Password or API key",
    tokenLabel: "Access token",
    defaultAuth: "token",
    countsByDefault: false,
    cli: "ant migrate artifactory",
  },
};

const slug = (name: string) =>
  name
    .toLowerCase()
    .replace(/[^a-z0-9_-]+/g, "-")
    .replace(/^[-_]+/, "")
    .slice(0, 63);

/** Items and unmapped entries of a report that belong to one source repository. */
export function attention(report: MigrationReport | undefined, name: string) {
  if (!report) return { conflicts: 0, notes: 0 };
  const mine = (n: string) =>
    n === name ||
    n === slug(name) ||
    n.startsWith(name + "/") ||
    n.startsWith(name + ":");
  return {
    conflicts: report.items.filter(
      (i) => i.action === "conflict" && mine(i.name),
    ).length,
    notes: report.unmapped.filter((u) => mine(u.name)).length,
  };
}

const unmapped = (r: MigrationRepository) => r.mapped_to === "unmapped";

function Source({
  client,
  kind,
  reloadRuns,
  active,
}: {
  client: Client;
  kind: SourceKind;
  reloadRuns: () => void;
  active: boolean;
}) {
  const spec = SPECS[kind];
  const [url, setUrl] = useState("");
  const [auth, setAuth] = useState<"basic" | "token">(spec.defaultAuth);
  const [user, setUser] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [caRef, setCaRef] = useState("");
  const [counts, setCounts] = useState(spec.countsByDefault);
  const [filter, setFilter] = useState("");
  const [found, setFound] = useState<MigrationReport>();
  const [selected, setSelected] = useState<Record<string, boolean>>({});
  const [report, setReport] = useState<MigrationReport>();
  const [reportKey, setReportKey] = useState("");
  const [drawer, setDrawer] = useState(false);
  const [compareOpen, setCompareOpen] = useState(false);
  const [coords, setCoords] = useState("");
  const [compare, setCompare] = useState<MigrationCompareReport>();
  const discover = useAction();
  const dry = useAction();
  const apply = useAction();
  const cmp = useAction();
  const toast = useToast();

  const creds = {
    url,
    ...(auth === "basic" && user ? { username: user } : {}),
    ...(auth === "basic" && password ? { password } : {}),
    ...(auth === "token" && token ? { token } : {}),
    ...(caRef ? { ca_ref: caRef } : {}),
  };
  const repos = useMemo(() => found?.repositories ?? [], [found]);
  const chosen = repos.filter((r) => selected[r.name]).map((r) => r.name);
  const key = `${url}\n${chosen.join(",")}`;
  const current = report?.dry_run && reportKey === key;
  const mappable = repos.filter((r) => !unmapped(r));
  const shown = repos.filter(
    (r) => !filter || r.name.toLowerCase().includes(filter.toLowerCase()),
  );
  const chosenRepos = repos.filter((r) => selected[r.name]);
  const totalBytes = chosenRepos.reduce((n, r) => n + (r.bytes ?? 0), 0);
  const view = report ?? found;
  const problems = view
    ? view.integrity_failures.length +
      Object.values(view.summary).reduce((n, m) => n + (m.conflict ?? 0), 0)
    : 0;

  const run = async (dry_run: boolean, only: string[]) => {
    const body = {
      ...creds,
      repositories: only,
      count_assets: counts,
      dry_run,
    };
    return (
      kind === "nexus"
        ? await client.call("migrateNexus", { body })
        : await client.call("migrateArtifactory", { body })
    ).data;
  };

  const doDiscover = async () => {
    const ok = await discover.run(async () => {
      const r = await run(true, []);
      setFound(r);
      setReport(undefined);
      setSelected(
        Object.fromEntries(r.repositories.map((x) => [x.name, !unmapped(x)])),
      );
    });
    if (ok) {
      toast(`${spec.label} inventory read`);
      reloadRuns();
    }
  };
  const doDry = async () => {
    const ok = await dry.run(async () => {
      setReport(await run(true, chosen));
      setReportKey(key);
      setDrawer(true);
    });
    if (ok) {
      toast("Migration dry run complete");
      reloadRuns();
    }
  };
  const doApply = async () => {
    const ok = await apply.run(async () => {
      setReport(await run(false, chosen));
      setDrawer(true);
    });
    if (ok) {
      toast("Definitions migrated");
      reloadRuns();
    }
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

  const allOn = mappable.length > 0 && mappable.every((r) => selected[r.name]);
  const someOn = mappable.some((r) => selected[r.name]);
  const busy = discover.busy || dry.busy || apply.busy;

  return (
    <div
      role="group"
      aria-label={`${spec.label} migration`}
      className="mig-src"
      hidden={!active}
    >
      <aside className="az-card card mig-conn">
        <div className="card__head">
          <h2>Connection</h2>
        </div>
        <Input
          label={spec.urlLabel}
          placeholder={spec.placeholder}
          value={url}
          onChange={val(setUrl)}
          autoComplete="off"
        />
        <SegmentedControl
          aria-label={`${spec.label} authentication`}
          block
          options={[
            { value: "basic", label: "User" },
            { value: "token", label: spec.tokenLabel },
          ]}
          value={auth}
          onChange={(v: string) => setAuth(v as "basic" | "token")}
        />
        {auth === "basic" ? (
          <>
            <Input
              label="Username"
              autoComplete="off"
              value={user}
              onChange={val(setUser)}
            />
            <Input
              label={spec.secretLabel}
              type="password"
              autoComplete="off"
              value={password}
              onChange={val(setPassword)}
            />
          </>
        ) : (
          <Input
            label={spec.tokenLabel}
            type="password"
            autoComplete="off"
            value={token}
            onChange={val(setToken)}
          />
        )}
        <details className="mig-more">
          <summary>Advanced</summary>
          <div className="stack">
            <Input
              label="CA bundle reference"
              hint="An operator-configured ID for a private certificate authority."
              value={caRef}
              onChange={val(setCaRef)}
            />
            <Switch
              label="Count assets per repository"
              labelPosition="end"
              checked={counts}
              onChange={chk(setCounts)}
            />
            <p className="muted">{spec.urlHint}</p>
          </div>
        </details>
        <details className="mig-more">
          <summary>Copy artifact content</summary>
          <p className="muted">
            Definitions are migrated here. To copy bytes, run on the server:{" "}
            <Mono>{spec.cli} --copy-content --apply</Mono>
          </p>
        </details>
        <Failure error={discover.error} />
        <Button icon="search" disabled={busy || !url} onClick={doDiscover}>
          {discover.busy ? "Reading…" : "Discover repositories"}
        </Button>
      </aside>

      <section className="az-card card mig-main">
        <div className="card__head">
          <h2>
            Repositories
            {repos.length > 0 && (
              <span className="muted"> · {repos.length} found</span>
            )}
          </h2>
          {repos.length > 0 && (
            <Input
              size="sm"
              aria-label="Filter repositories"
              placeholder="Filter"
              value={filter}
              onChange={val(setFilter)}
            />
          )}
        </div>
        {repos.length === 0 ? (
          <EmptyState
            title="No inventory yet"
            description={`Enter the ${spec.label} address and credentials, then discover its repositories. Nothing is changed until you apply.`}
          />
        ) : (
          <div className="mig-table">
            <Table
              rowKey="name"
              columns={[
                {
                  key: "sel",
                  width: 36,
                  primary: false,
                  label: (
                    <Checkbox
                      aria-label="Select all repositories"
                      checked={allOn}
                      indeterminate={someOn && !allOn}
                      onChange={(e: { target: { checked: boolean } }) =>
                        setSelected(
                          Object.fromEntries(
                            repos.map((r) => [
                              r.name,
                              !unmapped(r) && e.target.checked,
                            ]),
                          ),
                        )
                      }
                    />
                  ),
                  render: (r: MigrationRepository) => (
                    <Checkbox
                      aria-label={`Include ${r.name}`}
                      disabled={unmapped(r)}
                      checked={!!selected[r.name]}
                      onChange={(e: { target: { checked: boolean } }) =>
                        setSelected((s) => ({
                          ...s,
                          [r.name]: e.target.checked,
                        }))
                      }
                    />
                  ),
                },
                {
                  key: "name",
                  label: "Repository",
                  primary: false,
                  render: (r: MigrationRepository) => <Mono>{r.name}</Mono>,
                },
                {
                  key: "format",
                  label: "Format",
                  render: (r: MigrationRepository) => (
                    <span
                      title={r.ant_format ? `Ant format ${r.ant_format}` : ""}
                    >
                      {r.format}
                      {r.ant_format && r.ant_format !== r.format && (
                        <span className="muted"> → {r.ant_format}</span>
                      )}
                    </span>
                  ),
                },
                {
                  key: "type",
                  label: "Type",
                  render: (r: MigrationRepository) => (
                    <Badge tone="outline">{spec.types[r.type] ?? r.type}</Badge>
                  ),
                },
                {
                  key: "mapped_to",
                  label: "Maps to",
                  render: (r: MigrationRepository) =>
                    unmapped(r) ? (
                      <Badge tone="warn" dot>
                        not mapped
                      </Badge>
                    ) : (
                      <span title={r.mapped_to}>
                        {r.mapped_to.replace(
                          `+ feed ${slug(r.name)}`,
                          "+ feed",
                        )}
                      </span>
                    ),
                },
                {
                  key: "bytes",
                  label: "Size",
                  align: "right",
                  render: (r: MigrationRepository) =>
                    r.bytes ? bytes(r.bytes) : "—",
                },
                {
                  key: "assets",
                  label: "Assets",
                  align: "right",
                  render: (r: MigrationRepository) => count(r.assets),
                },
                {
                  key: "attention",
                  label: "Notes",
                  render: (r: MigrationRepository) => {
                    const a = attention(view, r.name);
                    return (
                      <span className="row tight">
                        {a.conflicts > 0 && (
                          <Badge tone="bad" dot>
                            {a.conflicts} conflict{a.conflicts > 1 ? "s" : ""}
                          </Badge>
                        )}
                        {a.notes > 0 && (
                          <Badge tone="warn" dot>
                            {a.notes} note{a.notes > 1 ? "s" : ""}
                          </Badge>
                        )}
                        {a.conflicts + a.notes === 0 && (
                          <span className="muted">—</span>
                        )}
                      </span>
                    );
                  },
                },
              ]}
              rows={shown}
            />
          </div>
        )}
        <div className="mig-bar">
          <div className="mig-bar__summary" aria-live="polite">
            {repos.length === 0 ? (
              <span className="muted">Discover repositories to begin.</span>
            ) : (
              <>
                <strong>{chosen.length}</strong> of {repos.length} selected
                <span className="muted"> · {bytes(totalBytes)}</span>
                {view && view.unmapped.length > 0 && (
                  <Badge tone="warn" dot>
                    {view.unmapped.length} need attention
                  </Badge>
                )}
                {problems > 0 && (
                  <Badge tone="bad" dot>
                    {problems} problem{problems > 1 ? "s" : ""}
                  </Badge>
                )}
                {current && (
                  <Badge tone="good" dot>
                    dry run current
                  </Badge>
                )}
              </>
            )}
          </div>
          <Failure error={dry.error ?? apply.error} />
          <div className="row">
            <Button
              variant="ghost"
              disabled={!url || busy}
              onClick={() => setCompareOpen(true)}
            >
              Compare…
            </Button>
            {report && (
              <Button variant="ghost" onClick={() => setDrawer(true)}>
                Report
              </Button>
            )}
            <Button
              variant="secondary"
              disabled={busy || !url || chosen.length === 0}
              onClick={doDry}
            >
              {dry.busy ? "Reading…" : "Dry run"}
            </Button>
            <Button
              disabled={busy || !current}
              title={current ? "" : "Run a dry run of this selection first"}
              onClick={doApply}
            >
              {apply.busy ? "Applying…" : "Migrate"}
            </Button>
          </div>
        </div>
      </section>

      {drawer && report && (
        <Drawer
          open
          title={report.dry_run ? "Dry run report" : "Migration applied"}
          description={`${spec.label} · ${report.repositories.length} repositories`}
          width={720}
          onClose={() => setDrawer(false)}
          footer={
            report.dry_run ? (
              <Button disabled={busy || !current} onClick={doApply}>
                {apply.busy ? "Applying…" : "Migrate"}
              </Button>
            ) : undefined
          }
        >
          <ReportView report={report} />
        </Drawer>
      )}

      {compareOpen && (
        <Modal
          open
          title="Compare during staged adoption"
          description={`What a client receives from ${spec.label} versus the Ant feed. One coordinate per line: repository path [feed].`}
          width={720}
          onClose={() => setCompareOpen(false)}
          actions={
            <>
              <Button variant="secondary" onClick={() => setCompareOpen(false)}>
                Close
              </Button>
              <Button
                disabled={cmp.busy || !url || !items.length}
                onClick={async () => {
                  const ok = await cmp.run(async () => {
                    setCompare(
                      (
                        await client.call("compareMigration", {
                          body: { ...creds, manager: kind, items },
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
            </>
          }
        >
          <div className="stack">
            <Textarea
              label="Coordinates"
              mono
              rows={4}
              value={coords}
              onChange={val(setCoords)}
            />
            <Failure error={cmp.error} />
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
                    label: spec.label,
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
          </div>
        </Modal>
      )}
    </div>
  );
}

/** One-page migration from another artifact manager, with a source switcher. */
export function MigratePanel({
  client,
  reloadRuns,
}: {
  client: Client;
  reloadRuns: () => void;
}) {
  const [kind, setKind] = useState<SourceKind>("nexus");
  return (
    <div className="mig-panel">
      <div className="mig-toolbar">
        <SegmentedControl
          aria-label="Migration source"
          options={[
            { value: "nexus", label: "Nexus" },
            { value: "artifactory", label: "Artifactory" },
          ]}
          value={kind}
          onChange={(v: string) => setKind(v as SourceKind)}
        />
        <span className="muted">
          Credentials are used for the request only and are never stored.
        </span>
      </div>
      {(Object.keys(SPECS) as SourceKind[]).map((k) => (
        <Source
          key={k}
          client={client}
          kind={k}
          reloadRuns={reloadRuns}
          active={kind === k}
        />
      ))}
    </div>
  );
}
