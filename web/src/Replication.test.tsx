// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import App from "./App";
import { ids, json, mockAPI, type Call } from "./testkit";

let calls: Call[];
function install(...args: Parameters<typeof mockAPI>) {
  const api = mockAPI(...args);
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const nav = (name: string) =>
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name }),
  );
const writes = (m: string) => calls.filter((c) => c.init.method === m);
const body = (c: Call) => JSON.parse(c.init.body as string);
const header = (c: Call, name: string) => new Headers(c.init.headers).get(name);

const session = {
  csrf_token: "a".repeat(43),
  session: {
    PrincipalID: ids.principal,
    InstanceID: "i",
    ExpiresAt: new Date(Date.now() + 3600000).toISOString(),
  },
};
const peer = {
  id: "00000000-0000-0000-0000-0000000000a1",
  name: "site-b",
  url: "https://b.example",
  ca_ref: "corp-ca",
  revision: 3,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
};
const status = {
  phase: "incremental",
  running: false,
  checkpoint: 40,
  remote_head: 42,
  changes_behind: 2,
  lag_seconds: 3,
  seed_done: 10,
  consecutive_failures: 0,
  assets_applied: 10,
  blobs_transferred: 4,
  blobs_deduplicated: 6,
  bytes_transferred: 4096,
  deletions_applied: 0,
  deletions_held: 1,
  conflicts: 0,
  failures_total: 5,
  reseeds: 2,
  reseeding: true,
  source_low_water: 33,
  backfills_pending: 1,
  backfills_completed: 3,
  resumed_bytes: 2048,
};
const job = {
  id: "00000000-0000-0000-0000-0000000000b1",
  name: "mirror",
  peer_id: peer.id,
  remote_feed_id: "00000000-0000-0000-0000-0000000000c1",
  dest_feed_id: ids.feed,
  replica_source_id: "00000000-0000-0000-0000-0000000000d1",
  mode: "scheduled",
  interval_seconds: 120,
  include_cached: true,
  filters: { namespaces: ["team"], version_prefixes: ["1."] },
  deletion_policy: "ignore",
  destination_read_only: true,
  enabled: true,
  revision: 7,
  created_at: "2026-01-01T00:00:00Z",
  status,
};
const preview = (blocked = 0) => ({
  digest: "d".repeat(64),
  template: {
    version: 1,
    format: "oci",
    remote_sources: [],
    sources: [],
    excluded: [],
  },
  diffs: [
    {
      section: "retention",
      field: "keep_last",
      action: "change",
      current: 10,
      proposed: 5,
    },
    {
      section: "retention",
      field: "allow_release_delete",
      action: "preserved",
      reason: "destination decides whether release content may be deleted",
    },
    ...(blocked
      ? [
          {
            section: "remote_source:up",
            field: "source",
            action: "blocked",
            reason: "no remote source named up",
          },
        ]
      : []),
  ],
  applicable: 1,
  blocked,
});

function setup(blocked = 0) {
  install((url, init) => {
    const path = url.split("?")[0];
    const method = init.method ?? "GET";
    if (path === "/auth/session") return json(session);
    if (path.startsWith("/api/v1/permissions/effective"))
      return json({
        principal_id: ids.principal,
        disabled: false,
        resource: { kind: "instance" },
        actions: [
          {
            action: "admin",
            allowed: true,
            via_roles: ["r"],
            denied_by_roles: [],
          },
          {
            action: "replicate",
            allowed: true,
            via_roles: ["r"],
            denied_by_roles: [],
          },
        ],
      });
    if (path === "/api/v1/replication/jobs" && method === "GET")
      return json({ items: [job] });
    if (path === "/api/v1/replication/peers" && method === "GET")
      return json({ items: [peer] });
    if (path === `/api/v1/replication/jobs/${job.id}/template/preview`)
      return json(preview(blocked));
    if (path === `/api/v1/replication/jobs/${job.id}/template/apply`)
      return json({ digest: "d".repeat(64), applied: 1, diffs: [] });
    return undefined;
  });
}
async function openReplication() {
  render(<App />);
  await screen.findByRole("navigation", { name: "Main navigation" });
  nav("Replication");
  await screen.findByText("mirror");
}
const rowMenu = async (label: string) => {
  fireEvent.click(await screen.findByRole("button", { name: label }));
};

it("shows reseed, back-fill, resume and failure counters in the job details", async () => {
  setup();
  await openReplication();
  expect(screen.getByText("Reseeding")).toBeTruthy();
  fireEvent.click(screen.getByText("mirror"));
  const drawer = await screen.findByRole("dialog");
  expect(within(drawer).getByText(/peer purged change history/i)).toBeTruthy();
  expect(within(drawer).getByText(/5 total/)).toBeTruthy();
  expect(within(drawer).getByText(/peer history starts after 33/)).toBeTruthy();
  expect(within(drawer).getByText("1 pending · 3 done")).toBeTruthy();
  expect(within(drawer).getByText("namespace team, version 1.*")).toBeTruthy();
});

it("edits a job through the API with filters split per line", async () => {
  setup();
  await openReplication();
  await rowMenu("Actions for mirror");
  fireEvent.click(await screen.findByText("Edit job"));
  const dialog = await screen.findByRole("dialog");
  fireEvent.change(within(dialog).getByLabelText("Interval (seconds)"), {
    target: { value: "30" },
  });
  fireEvent.change(within(dialog).getByLabelText("Packages"), {
    target: { value: "team/a\nteam/b, team/a" },
  });
  fireEvent.click(within(dialog).getByLabelText("Propagate deletions"));
  fireEvent.click(within(dialog).getByLabelText(/Read-only replica/));
  fireEvent.click(within(dialog).getByRole("button", { name: "Save job" }));
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  const call = writes("PATCH")[0];
  expect(call.url).toBe(`/api/v1/replication/jobs/${job.id}`);
  expect(header(call, "Idempotency-Key")).toBeTruthy();
  expect(body(call)).toEqual({
    revision: 7,
    mode: "scheduled",
    interval_seconds: 30,
    include_cached: true,
    deletion_policy: "propagate",
    destination_read_only: false,
    filters: {
      namespaces: ["team"],
      packages: ["team/a", "team/b"],
      version_prefixes: ["1."],
    },
  });
});

it("rotates a peer token without echoing it", async () => {
  setup();
  await openReplication();
  fireEvent.click(screen.getByRole("tab", { name: /Peers/ }));
  await rowMenu("Actions for site-b");
  fireEvent.click(await screen.findByText("Rotate token"));
  const dialog = await screen.findByRole("dialog");
  const token = "n".repeat(24);
  fireEvent.change(within(dialog).getByLabelText("New access token"), {
    target: { value: token },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Rotate token" }));
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  const call = writes("PATCH")[0];
  expect(call.url).toBe(`/api/v1/replication/peers/${peer.id}`);
  expect(header(call, "Idempotency-Key")).toBeTruthy();
  expect(body(call)).toEqual({ revision: 3, token });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.body.textContent).not.toContain(token);
});

it("reviews a configuration template and applies exactly the reviewed digest", async () => {
  setup();
  await openReplication();
  await rowMenu("Actions for mirror");
  fireEvent.click(await screen.findByText("Configuration template"));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("keep_last");
  expect(within(dialog).queryByText("allow_release_delete")).toBeNull();
  fireEvent.click(
    within(dialog).getByLabelText(/Show unchanged and preserved/),
  );
  expect(within(dialog).getByText("allow_release_delete")).toBeTruthy();
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Apply reviewed template" }),
  );
  await waitFor(() =>
    expect(writes("POST").some((c) => c.url.endsWith("/template/apply"))).toBe(
      true,
    ),
  );
  const apply = writes("POST").find((c) => c.url.endsWith("/template/apply"))!;
  expect(body(apply)).toEqual({ digest: "d".repeat(64) });
  expect(header(apply, "Idempotency-Key")).toBeTruthy();
  await within(dialog).findByText("Template applied");
});

it("refuses to apply a template with blocked entries", async () => {
  setup(1);
  await openReplication();
  await rowMenu("Actions for mirror");
  fireEvent.click(await screen.findByText("Configuration template"));
  const dialog = await screen.findByRole("dialog");
  await within(dialog).findByText("This template cannot be applied");
  const apply = within(dialog).getByRole("button", {
    name: "Apply reviewed template",
  }) as HTMLButtonElement;
  expect(apply.disabled).toBe(true);
  expect(writes("POST").some((c) => c.url.endsWith("/template/apply"))).toBe(
    false,
  );
});

const outgoingMember = {
  id: "f1:member:m1",
  consumer_kind: "member",
  consumer_name: "kw",
  member_instance_id: "m1",
  feed_id: ids.feed,
  feed_slug: "production",
  feed_format: "oci",
  remote_job: "pull-production",
  rule_id: "99999999-8888-4777-8666-555555555555",
  first_seen: "2026-10-01T00:00:00Z",
  last_seen: "2026-10-20T00:00:00Z",
  pulls: 12,
  bytes_served: 2048,
  last_sequence: 90,
  head_sequence: 100,
  lag_changes: 10,
  lag_seconds: 30,
  status: "active",
  stale_after_seconds: 3600,
  history: [
    {
      at: "2026-10-19T23:50:00Z",
      last_sequence: 80,
      head_sequence: 100,
      lag_changes: 20,
    },
    {
      at: "2026-10-19T23:55:00Z",
      last_sequence: 90,
      head_sequence: 100,
      lag_changes: 10,
    },
  ],
};
const outgoingToken = {
  ...outgoingMember,
  id: "f1:principal:p1",
  consumer_kind: "principal",
  consumer_name: "peer-token",
  member_instance_id: undefined,
  remote_job: undefined,
  rule_id: undefined,
  status: "error",
  last_error: "history purged; reseed required",
};

function setupUnified(federated: boolean) {
  install((url, init) => {
    const path = url.split("?")[0];
    if (path === "/api/v1/replication/outgoing")
      return json({ items: [outgoingMember, outgoingToken] });
    if (path === "/api/v1/federation/status")
      return json(
        federated
          ? { state: "active", role: "authority", federation_name: "lab" }
          : { state: "none" },
      );
    if (path === "/api/v1/federation/members") return json({ items: [] });
    if (path === "/api/v1/federation/topology")
      return json({ generation: 1, authority: true, feeds: [], rules: [] });
    if (path === "/api/v1/federation/replication/status")
      return json({ items: [], quarantines: [] });
    if (path.startsWith("/auth/session")) return json(session);
    if (path === "/api/v1/replication/jobs" && (init.method ?? "GET") === "GET")
      return json({ items: [job] });
    if (path === "/api/v1/replication/peers") return json({ items: [peer] });
    return undefined;
  });
}

it("lists incoming and outgoing replication with explicit direction and filters", async () => {
  setupUnified(false);
  await openReplication();
  expect(screen.getAllByText("Incoming").length).toBeGreaterThan(0);
  await screen.findByText("kw");
  expect(screen.getAllByText("Outgoing").length).toBeGreaterThan(0);
  expect(screen.queryByRole("tab", { name: /Federation rules/ })).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: /^Outgoing/ }));
  expect(screen.queryByText("mirror")).toBeNull();
  expect(screen.getByText("kw")).toBeTruthy();
  fireEvent.click(screen.getByRole("radio", { name: /^Incoming/ }));
  expect(screen.getByText("mirror")).toBeTruthy();
  expect(screen.queryByText("kw")).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: /^All/ }));
  fireEvent.change(screen.getByLabelText("Status"), {
    target: { value: "error" },
  });
  expect(screen.queryByText("mirror")).toBeNull();
  expect(screen.getByText("peer-token")).toBeTruthy();
});

it("opens a read-only drawer for an outgoing consumer", async () => {
  setupUnified(false);
  await openReplication();
  fireEvent.click(await screen.findByText("kw"));
  const drawer = await screen.findByRole("dialog");
  expect(within(drawer).getByText(/Pulled by the consumer/)).toBeTruthy();
  expect(within(drawer).getByText("90 of 100")).toBeTruthy();
  expect(within(drawer).getByText("pull-production")).toBeTruthy();
  expect(within(drawer).queryByText("Edit job")).toBeNull();
});

it("shows the Federation rules tab only when federated", async () => {
  setupUnified(true);
  await openReplication();
  fireEvent.click(await screen.findByRole("tab", { name: /Federation rules/ }));
  await screen.findByText("Federated feeds");
});
