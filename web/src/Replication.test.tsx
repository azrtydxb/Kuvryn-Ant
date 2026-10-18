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
