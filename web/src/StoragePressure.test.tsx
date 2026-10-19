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
import { json, mockAPI, signInWithToken, type Call } from "./testkit";

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

const pressure = (over: Record<string, unknown> = {}) => ({
  enabled: true,
  state: "pressure",
  capacity_source: "quota",
  used_bytes: 900 * 2 ** 30,
  capacity_bytes: 1000 * 2 ** 30,
  usage_ratio: 0.9,
  high_percent: 85,
  low_percent: 75,
  reclaimable_bytes: 2 ** 30,
  last_evicted_versions: 4,
  last_evicted_bytes: 2 ** 30,
  total_evicted_versions: 40,
  total_evicted_bytes: 10 * 2 ** 30,
  last_evicted_at: "2026-01-01T00:00:00Z",
  ...over,
});
const usage = (p: unknown) => ({
  backend: "s3",
  blob_count: 3,
  blob_bytes: 900 * 2 ** 30,
  logical_bytes: 900 * 2 ** 30,
  dedup_saved_bytes: 0,
  unreferenced_bytes: 5 * 2 ** 30,
  gc_candidates: 0,
  reclaimable_bytes: 2 ** 30,
  in_grace_bytes: 4 * 2 ** 30,
  domains: [],
  measured_at: "2026-01-01T00:00:00Z",
  pressure: p,
});
const settings = {
  revision: 3,
  default_policy: { enabled: false, min_keep: 1, include_tagged: false },
  pressure: { enabled: true, high_percent: 85, low_percent: 75 },
  locked: ["pressure.high_percent"],
  gc: {
    interval_seconds: 21600,
    grace_seconds: 86400,
    dry_run: false,
    pressure_interval_seconds: 300,
    pressure_sweep_grace_seconds: 900,
  },
};

async function openRetention() {
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Retention/ }));
}

it("shows usage against capacity, watermarks and why unreferenced bytes are not yet candidates", async () => {
  install((url) => {
    if (url === "/api/v1/storage/usage") return json(usage(pressure()));
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return undefined;
  });
  await openRetention();
  expect(await screen.findByText("Above high watermark")).toBeTruthy();
  expect(screen.getByText(/high 85% · low 75%/)).toBeTruthy();
  expect(screen.getByText(/still in the grace period/)).toBeTruthy();
});

it("explains that object storage without a quota has no pressure trigger", async () => {
  install((url) => {
    if (url === "/api/v1/storage/usage")
      return json(
        usage(
          pressure({
            state: "no_capacity",
            capacity_source: "none",
            capacity_bytes: 0,
            used_bytes: 0,
            usage_ratio: 0,
          }),
        ),
      );
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return undefined;
  });
  await openRetention();
  expect(
    await screen.findByText("Pressure eviction is off for this backend"),
  ).toBeTruthy();
});

it("warns when pressure has nothing evictable and starts a dry run from the overview", async () => {
  install((url, init) => {
    if (url === "/api/v1/storage/usage")
      return json(usage(pressure({ state: "unevictable" })));
    if (url.startsWith("/api/v1/retention/runs") && init.method === "POST")
      return json({
        id: "r1",
        trigger: "manual",
        dry_run: true,
        state: "succeeded",
        started_at: "2026-01-01T00:00:00Z",
        report: {
          versions_deleted: 0,
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
          pressure: {
            dry_run: true,
            state: "pressure",
            capacity_source: "quota",
            used_bytes: 10,
            capacity_bytes: 100,
            usage_ratio: 0.9,
            high_percent: 85,
            low_percent: 75,
            reclaimable_bytes: 0,
            target_bytes: 15,
            evicted_versions: 3,
            evicted_bytes: 15,
            shortfall_bytes: 0,
            expedited: 0,
            swept: 0,
            evicted: [],
          },
        },
      });
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return undefined;
  });
  await openRetention();
  expect(
    await screen.findByText("Storage is full and nothing is evictable"),
  ).toBeTruthy();
  fireEvent.click(await screen.findByRole("button", { name: "Dry run" }));
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
  expect(await screen.findByText("Would evict")).toBeTruthy();
});

it("saves instance defaults with the revision and disables environment-locked fields", async () => {
  install((url) => {
    if (url === "/api/v1/storage/usage") return json(usage(pressure()));
    if (url === "/api/v1/retention/settings") return json(settings, 200, '"3"');
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return undefined;
  });
  await openRetention();
  fireEvent.click(await screen.findByRole("tab", { name: "Defaults" }));
  const high = await screen.findByLabelText("High watermark (%)");
  expect((high as HTMLInputElement).disabled).toBe(true);
  fireEvent.click(screen.getByLabelText("Enable the default policy"));
  fireEvent.change(
    screen.getByLabelText("Delete when not accessed for (days)"),
    {
      target: { value: "30" },
    },
  );
  fireEvent.change(screen.getByLabelText("Capacity quota (GiB)"), {
    target: { value: "500" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save settings" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const put = writes("PUT")[0];
  expect(new Headers(put.init.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(put.init.body as string)).toEqual({
    default_policy: {
      enabled: true,
      max_idle_seconds: 30 * 86400,
      min_keep: 1,
      include_tagged: false,
    },
    pressure: {
      enabled: true,
      high_percent: 85,
      low_percent: 75,
      capacity_bytes: 500 * 2 ** 30,
    },
  });
});
