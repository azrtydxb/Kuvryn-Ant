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
import { ids, json, mockAPI, page, signInWithToken } from "./testkit";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const feeds = Array.from({ length: 8 }, (_, i) => ({
  id: `00000000-0000-0000-0000-00000000f00${i}`,
  slug: `feed-${i}`,
  format: "npm",
  revision: 1,
  anonymous_read: false,
  purpose: "artifacts",
}));
const sources = Array.from({ length: 8 }, (_, i) => ({
  id: `00000000-0000-0000-0000-00000000a00${i}`,
  name: `source-${i}`,
  kind: "remote",
  format: "npm",
  revision: 1,
}));
const zero = {
  pulls: 0,
  publishes: 0,
  bytes_out: 0,
  bytes_in: 0,
  cache_hits: 0,
  cache_misses: 0,
  errors: 0,
};
const storage = {
  backend: "disk",
  blob_count: 3,
  blob_bytes: 1000,
  logical_bytes: 2000,
  dedup_saved_bytes: 1000,
  unreferenced_bytes: 0,
  gc_candidates: 0,
  domains: [],
  cache_feeds: [],
  measured_at: "2026-01-01T00:00:00Z",
  pressure: {
    enabled: true,
    state: "ok",
    capacity_source: "filesystem",
    used_bytes: 500,
    capacity_bytes: 1000,
    usage_ratio: 0.5,
    high_percent: 90,
    low_percent: 80,
    reclaimable_bytes: 0,
    last_evicted_versions: 0,
    last_evicted_bytes: 0,
    total_evicted_versions: 0,
    total_evicted_bytes: 0,
  },
};
const job = {
  id: "00000000-0000-0000-0000-0000000000b1",
  name: "mirror-eu",
  dest_feed_id: ids.feed,
  enabled: true,
  status: {
    phase: "incremental",
    running: false,
    checkpoint: 1,
    changes_behind: 7,
    seed_done: 0,
    consecutive_failures: 0,
  },
};

function start(failing: boolean) {
  const api = mockAPI((url) => {
    const path = url.split("?")[0];
    if (path === "/auth/session") return undefined;
    if (path === "/api/v1/feeds") return json({ items: feeds, page });
    if (path === "/api/v1/sources") return json({ items: sources, page });
    if (path === "/api/v1/stats/feeds")
      return json({
        items: feeds.map((f, i) => ({
          feed_id: f.id,
          slug: f.slug,
          format: f.format,
          packages: 10 + i,
          versions: 1,
          assets: 1,
          bytes: 1,
          pulls: i,
        })),
        page,
      });
    if (path === "/api/v1/source-health")
      return json({
        items: sources.map((s, i) => ({
          source_id: s.id,
          kind: "remote",
          status: failing && i < 4 ? "failing" : "healthy",
          requests: 1,
          failures: 0,
          ...(failing && i < 4 ? { last_status_code: 503 } : {}),
        })),
        page,
      });
    if (path === "/api/v1/storage/usage") return json(storage);
    if (path === "/api/v1/replication/status") return json({ items: [job] });
    if (path === "/api/v1/metrics/traffic")
      return json({
        scope: "instance",
        from: "",
        to: "",
        step_seconds: 3600,
        points: [],
        totals: { ...zero, pulls: 5, publishes: 1 },
      });
    if (path === "/api/v1/audit/events") return json({ items: [], page });
    return undefined;
  });
  vi.stubGlobal("fetch", api.fetcher);
  render(<App />);
  return signInWithToken();
}
const card = (name: string) =>
  screen.getByRole("heading", { name, level: 2 }).closest("section")!;

it("shows the dense dashboard cards with capped rows and view-all links", async () => {
  await start(false);
  const feedsCard = await waitFor(() => {
    const c = card("Feeds");
    expect(within(c).getByText("feed-7")).toBeTruthy();
    return c;
  });
  // Busiest feeds first, never more than five rows.
  expect(within(feedsCard).queryByText("feed-0")).toBeNull();
  expect(within(feedsCard).getAllByRole("row")).toHaveLength(1 + 5);
  expect(within(card("Sources")).getAllByRole("row")).toHaveLength(1 + 5);
  expect(screen.getByRole("heading", { name: "Replication" })).toBeTruthy();
  expect(
    await within(card("Replication")).findByText("mirror-eu"),
  ).toBeTruthy();
  expect(within(card("Storage")).getByText(/Pressure \(ok\)/)).toBeTruthy();
  // Nothing is failing, so the failing card is not taking space.
  expect(screen.queryByRole("heading", { name: "Failing sources" })).toBeNull();
  fireEvent.click(within(feedsCard).getByRole("button", { name: /View all/ }));
  expect(
    await screen.findByRole("heading", { level: 1, name: "Feeds" }),
  ).toBeTruthy();
});

it("lists failing sources with a link to each and caps the list", async () => {
  await start(true);
  const failing = await waitFor(() => card("Failing sources"));
  await waitFor(() =>
    expect(
      within(failing).getAllByRole("button", { name: "Open source" }),
    ).toHaveLength(3),
  );
  expect(within(failing).getByRole("button", { name: "1 more" })).toBeTruthy();
  expect(within(failing).getAllByText(/HTTP 503/)).toHaveLength(3);
});
