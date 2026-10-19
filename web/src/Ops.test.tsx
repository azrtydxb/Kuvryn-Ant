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
import {
  ids,
  json,
  mockAPI,
  page,
  signInWithToken,
  type Call,
} from "./testkit";

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

it("shows readiness, a failing upstream and storage on the overview", async () => {
  install((url) => {
    if (url === "/api/v1/status")
      return json({
        name: "kuvryn-ant",
        api_version: "v1",
        readiness: {
          ready: true,
          checked_at: "2026-01-01T00:00:00Z",
          database: { status: "ok", latency_ms: 3 },
          blob_storage: { status: "ok" },
          writer_lease: { state: "held" },
        },
      });
    if (url.startsWith("/api/v1/source-health"))
      return json({
        items: [
          {
            source_id: ids.source,
            kind: "remote",
            status: "failing",
            last_status_code: 401,
            requests: 5,
            failures: 5,
          },
        ],
        page,
      });
    if (url === "/api/v1/storage/usage")
      return json({
        backend: "s3",
        blob_count: 4,
        blob_bytes: 1024,
        logical_bytes: 4096,
        dedup_saved_bytes: 3072,
        unreferenced_bytes: 0,
        gc_candidates: 0,
        domains: [],
        measured_at: "2026-01-01T00:00:00Z",
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  await screen.findByText("oci-local cannot reach its upstream");
  await screen.findByText(/Ready · 3 ms/);
  await screen.findByText(/deduplication saved/);
});
it("tests a remote source connection and shows the result", async () => {
  install((url, init) => {
    if (url === "/api/v1/sources?limit=200")
      return json({
        items: [
          {
            id: ids.source,
            name: "mirror",
            kind: "remote",
            format: "oci",
            revision: 1,
          },
        ],
        page,
      });
    if (url.endsWith("/remote-config")) return json({}, 404);
    if (url.endsWith(":test") && init.method === "POST")
      return json({
        source_id: ids.source,
        reachable: true,
        authenticated: false,
        status_code: 401,
        latency_ms: 12,
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Sources");
  fireEvent.click(await screen.findByRole("button", { name: /mirror/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Upstream" }));
  fireEvent.click(
    await screen.findByRole("button", { name: "Test connection" }),
  );
  await screen.findByText(/reachable but not authenticated \(HTTP 401\)/);
});
it("saves instance settings with the settings revision", async () => {
  install((url, init) => {
    if (url === "/api/v1/settings" && init.method === "PUT")
      return json({ revision: 4, items: [] });
    if (url === "/api/v1/settings")
      return json({
        revision: 3,
        items: [
          {
            key: "session_max_age_seconds",
            value: "3600",
            source: "stored",
            read_only: false,
            enforced: true,
          },
          {
            key: "session_idle_timeout_seconds",
            value: "900",
            source: "stored",
            read_only: false,
            enforced: true,
          },
          {
            key: "default_anonymous_read",
            value: "false",
            source: "default",
            read_only: false,
            enforced: false,
          },
        ],
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: "Security" }));
  fireEvent.change(await screen.findByLabelText(/Idle timeout/), {
    target: { value: "20" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const call = writes("PUT")[0];
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    session_max_age_seconds: 3600,
    session_idle_timeout_seconds: 1200,
    default_anonymous_read: false,
  });
});
it("starts a retention dry run", async () => {
  install((url, init) => {
    if (url.startsWith("/api/v1/retention/policies"))
      return json({ items: [] });
    if (url.startsWith("/api/v1/retention/runs") && init.method === "POST")
      return json({
        id: "r1",
        trigger: "manual",
        dry_run: true,
        state: "succeeded",
        started_at: "2026-01-01T00:00:00Z",
        report: {
          versions_deleted: 2,
          protected_release: 0,
          protected_tagged: 0,
          blobs_marked: 0,
          blobs_swept: 0,
          blobs_unmarked: 0,
          skipped_referenced: 0,
          gc_failed: 0,
          integrity_checked: 0,
          integrity_bytes: 0,
          integrity_errors: 0,
          integrity_mismatches: 0,
          integrity_wrapped: false,
          truncated: false,
        },
      });
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Retention/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Runs" }));
  fireEvent.click(await screen.findByRole("button", { name: "Start run" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Start retention run",
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Start dry run" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    dry_run: true,
  });
});
it("hides admin-only areas when the server says the principal is not an admin, and shows replication jobs", async () => {
  const session = {
    csrf_token: "a".repeat(43),
    session: {
      PrincipalID: ids.principal,
      InstanceID: "i",
      ExpiresAt: new Date(Date.now() + 3600000).toISOString(),
    },
  };
  install((url) => {
    if (url === "/auth/session") return json(session);
    if (url.startsWith("/api/v1/permissions/effective"))
      return json({
        principal_id: ids.principal,
        disabled: false,
        resource: { kind: "instance" },
        actions: [
          {
            action: "admin",
            allowed: false,
            via_roles: [],
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
    if (url.startsWith("/api/v1/replication/jobs")) return json({ items: [] });
    if (url.startsWith("/api/v1/replication/peers")) return json({ items: [] });
    return undefined;
  });
  render(<App />);
  const sidebar = await screen.findByRole("navigation", {
    name: "Main navigation",
  });
  await waitFor(() =>
    expect(
      within(sidebar).queryByRole("button", { name: "Access" }),
    ).toBeNull(),
  );
  expect(
    within(sidebar).queryByRole("button", { name: "Settings" }),
  ).toBeNull();
  nav("Replication");
  await screen.findByText("No replication jobs");
});
