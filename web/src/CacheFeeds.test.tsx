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
  feed,
  ids,
  json,
  mockAPI,
  page,
  signInWithToken,
  type Call,
} from "./testkit";
import {
  draftFrom,
  draftToInput,
  idleLabel,
  policySummary,
} from "./CachePolicy";

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

const cacheFeed = {
  ...feed,
  id: "00000000-0000-0000-0000-0000000000c1",
  slug: "buildcache",
  purpose: "cache",
  cache_policy: { max_idle_seconds: 7 * 86400, max_bytes: 50 * 2 ** 30 },
};
const plainFeed = { ...feed, purpose: "artifacts" };

it("formats the cache policy", () => {
  expect(idleLabel(7 * 86400)).toBe("7 days");
  expect(idleLabel(86400)).toBe("1 day");
  expect(idleLabel(36 * 3600)).toBe("36 hours");
  expect(idleLabel(90 * 60)).toBe("2 hours");
  expect(idleLabel(300)).toBe("5 minutes");
  expect(
    policySummary({ max_idle_seconds: 7 * 86400, max_bytes: 5 * 2 ** 30 }),
  ).toBe("Evicted after 7 days idle · limited to 5.0 GB");
  expect(policySummary({ max_idle_seconds: 86400 })).toBe(
    "Evicted after 1 day idle · no size limit",
  );
  expect(draftToInput({ days: "7", gib: "" })).toEqual({
    max_idle_seconds: 604800,
    max_bytes: 0,
  });
  expect(draftToInput({ days: "0.5", gib: "2" })).toEqual({
    max_idle_seconds: 43200,
    max_bytes: 2 * 2 ** 30,
  });
  expect(draftToInput({ days: "0", gib: "" })).toBeUndefined();
  expect(draftToInput({ days: "1", gib: "-1" })).toBeUndefined();
  expect(
    draftFrom({ max_idle_seconds: 2 * 86400, max_bytes: 2 ** 30 }),
  ).toEqual({ days: "2", gib: "1" });
});

it("lists cache feeds with a Cache badge and filters by purpose", async () => {
  install((url) =>
    url.startsWith("/api/v1/feeds?")
      ? json({
          items: url.includes("purpose=cache")
            ? [cacheFeed]
            : [plainFeed, cacheFeed],
          page,
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  await screen.findByText("buildcache");
  expect(screen.getAllByText("Cache")).toHaveLength(1);
  expect(screen.getByText("production")).toBeTruthy();
  fireEvent.change(screen.getByLabelText("Filter by purpose"), {
    target: { value: "cache" },
  });
  await waitFor(() =>
    expect(calls.some((c) => c.url.includes("purpose=cache"))).toBe(true),
  );
  await waitFor(() => expect(screen.queryByText("production")).toBeNull());
  expect(screen.getByText("buildcache")).toBeTruthy();
});

it("creates a cache feed: purpose selector limits formats and sends the cache policy", async () => {
  install((url, init) =>
    url === "/api/v1/feeds/batch" && init.method === "POST"
      ? json({
          feed: { ...cacheFeed, id: ids.feed },
          sources: [],
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: "Create feed" }));
  const dialog = screen.getByRole("dialog", { name: "Create feed" });
  fireEvent.change(within(dialog).getByLabelText("Feed slug"), {
    target: { value: "buildcache" },
  });
  // Artifacts feeds offer every format.
  expect(within(dialog).getByRole("option", { name: "npm" })).toBeTruthy();
  fireEvent.change(within(dialog).getByLabelText("Format"), {
    target: { value: "npm" },
  });
  fireEvent.change(within(dialog).getByLabelText("Purpose"), {
    target: { value: "cache" },
  });
  // A cache feed can only be oci or raw; the incompatible choice is reset.
  expect(within(dialog).queryByRole("option", { name: "npm" })).toBeNull();
  expect(
    (within(dialog).getByLabelText("Format") as HTMLSelectElement).value,
  ).toBe("oci");
  expect(within(dialog).getByRole("option", { name: "raw" })).toBeTruthy();
  fireEvent.change(within(dialog).getByLabelText(/Evict when idle for/), {
    target: { value: "3" },
  });
  fireEvent.change(within(dialog).getByLabelText(/Size limit/), {
    target: { value: "20" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  within(dialog).getByText(/Build cache · Evicted after 3 days idle/);
  fireEvent.click(within(dialog).getByRole("button", { name: "Create feed" }));
  await waitFor(() =>
    expect(writes("POST").some((c) => c.url === "/api/v1/feeds/batch")).toBe(
      true,
    ),
  );
  const body = JSON.parse(
    writes("POST").find((c) => c.url === "/api/v1/feeds/batch")!.init
      .body as string,
  );
  expect(body).toMatchObject({
    slug: "buildcache",
    format: "oci",
    purpose: "cache",
    cache_policy: { max_idle_seconds: 3 * 86400, max_bytes: 20 * 2 ** 30 },
  });
});

it("shows the Cache badge and policy on the feed detail and saves policy changes", async () => {
  install((url, init) => {
    if (url.startsWith("/api/v1/feeds?"))
      return json({ items: [{ ...cacheFeed, id: ids.feed }], page });
    if (url === `/api/v1/feeds/${ids.feed}` && init.method !== "PATCH")
      return json({ ...cacheFeed, id: ids.feed });
    if (url === `/api/v1/feeds/${ids.feed}` && init.method === "PATCH")
      return json({ ...cacheFeed, id: ids.feed, revision: 3 });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /buildcache/ }));
  await screen.findByText(/Evicted after 7 days idle · limited to 50.0 GB/);
  expect(screen.getAllByText("Cache").length).toBeGreaterThan(0);
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  const idle = await screen.findByLabelText(/Evict when idle for/);
  expect((idle as HTMLInputElement).value).toBe("7");
  fireEvent.change(idle, { target: { value: "2" } });
  fireEvent.change(screen.getByLabelText(/Size limit/), {
    target: { value: "" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save feed" }));
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  expect(JSON.parse(writes("PATCH")[0].init.body as string)).toEqual({
    slug: "buildcache",
    anonymous_read: false,
    cache_policy: { max_idle_seconds: 2 * 86400, max_bytes: 0 },
  });
  expect(new Headers(writes("PATCH")[0].init.headers).get("If-Match")).toBe(
    '"2"',
  );
});

it("converting an artifacts feed to a cache feed sends the purpose", async () => {
  install((url, init) => {
    if (url === `/api/v1/feeds/${ids.feed}` && init.method === "PATCH")
      return json({ ...cacheFeed, id: ids.feed, revision: 3 });
    if (url === `/api/v1/feeds/${ids.feed}`) return json(plainFeed);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  fireEvent.change(await screen.findByLabelText("Purpose"), {
    target: { value: "cache" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save feed" }));
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  expect(JSON.parse(writes("PATCH")[0].init.body as string)).toMatchObject({
    purpose: "cache",
    cache_policy: { max_idle_seconds: 7 * 86400, max_bytes: 0 },
  });
});

it("only offers the cache purpose for oci and raw feeds", async () => {
  install((url) =>
    url === `/api/v1/feeds/${ids.feed}`
      ? json({ ...plainFeed, format: "npm" })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  expect(
    (await screen.findByLabelText("Purpose")) as HTMLSelectElement,
  ).toHaveProperty("disabled", true);
  screen.getByText("Only oci and raw feeds can be build caches.");
});

it("shows the bytes each cache feed holds on the retention overview", async () => {
  install((url) => {
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return url === "/api/v1/storage/usage"
      ? json({
          backend: "disk",
          blob_count: 3,
          blob_bytes: 100 * 2 ** 30,
          logical_bytes: 100 * 2 ** 30,
          dedup_saved_bytes: 0,
          unreferenced_bytes: 0,
          gc_candidates: 0,
          reclaimable_bytes: 0,
          in_grace_bytes: 0,
          domains: [],
          measured_at: "2026-01-01T00:00:00Z",
          cache_feeds: [
            {
              feed_id: cacheFeed.id,
              slug: "buildcache",
              format: "oci",
              bytes: 12 * 2 ** 30,
              versions: 340,
              max_idle_seconds: 7 * 86400,
              max_bytes: 50 * 2 ** 30,
            },
            {
              feed_id: "00000000-0000-0000-0000-0000000000c2",
              slug: "sccache",
              format: "raw",
              bytes: 3 * 2 ** 30,
              versions: 9000,
              max_idle_seconds: 3 * 86400,
            },
          ],
        })
      : undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Retention/ }));
  await screen.findByText("Build caches");
  const row = (slug: string) => screen.getByText(slug).closest("tr")!;
  expect(within(row("buildcache")).getByText("12.0 GB")).toBeTruthy();
  expect(within(row("buildcache")).getByText("50.0 GB")).toBeTruthy();
  expect(within(row("buildcache")).getByText("7 days")).toBeTruthy();
  expect(within(row("sccache")).getByText("3.0 GB")).toBeTruthy();
  expect(within(row("sccache")).getByText("No limit")).toBeTruthy();
  expect(screen.getByText(/2 cache feeds · 15.0 GB/)).toBeTruthy();
});

it("keeps the retention overview unchanged without cache feeds", async () => {
  install((url) => {
    if (url.startsWith("/api/v1/retention/")) return json({ items: [] });
    return url === "/api/v1/storage/usage"
      ? json({
          backend: "disk",
          blob_count: 1,
          blob_bytes: 1,
          logical_bytes: 1,
          dedup_saved_bytes: 0,
          unreferenced_bytes: 0,
          gc_candidates: 0,
          domains: [],
          measured_at: "2026-01-01T00:00:00Z",
          cache_feeds: [],
        })
      : undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Retention/ }));
  await screen.findByText("Storage pressure");
  expect(screen.queryByText("Build caches")).toBeNull();
});
