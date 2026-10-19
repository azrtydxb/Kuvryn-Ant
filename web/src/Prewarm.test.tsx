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
import { ids, json, mockAPI, signInWithToken, type Call } from "./testkit";

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

const formats = {
  items: [
    {
      format: "oci",
      packages: false,
      images: true,
      feed: false,
      lockfiles: [],
    },
    {
      format: "npm",
      packages: true,
      images: false,
      feed: true,
      lockfiles: ["package-lock.json", "pnpm-lock.yaml"],
    },
  ],
};
const run = {
  id: "00000000-0000-0000-0000-0000000000b1",
  job_id: "00000000-0000-0000-0000-0000000000a1",
  feed_id: ids.feed,
  trigger: "manual",
  state: "partial",
  cancel_requested: false,
  principal_id: ids.principal,
  total: 4,
  warmed: 2,
  cached: 1,
  failed: 1,
  skipped: 0,
  bytes: 2048,
  created_at: "2026-01-01T00:00:00Z",
  started_at: "2026-01-01T00:00:01Z",
  finished_at: "2026-01-01T00:00:09Z",
};
const job = {
  id: run.job_id,
  feed_id: ids.feed,
  name: "base images",
  source_kind: "images",
  item_count: 4,
  options: { platforms: ["linux/amd64"] },
  concurrency: 4,
  rate_per_second: 10,
  interval_seconds: 86400,
  enabled: true,
  next_run_at: "2026-01-02T00:00:00Z",
  revision: 1,
  created_by: ids.principal,
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-01T00:00:00Z",
  last_run: run,
};
const handlers = (url: string, init: RequestInit) => {
  if (url === "/api/v1/prewarm/formats") return json(formats);
  if (url.startsWith("/api/v1/prewarm/jobs?")) return json({ items: [job] });
  if (url === "/api/v1/prewarm/jobs" && init.method === "POST")
    return json({ ...job, name: "new job", last_run: undefined }, 201, '"1"');
  if (url === `/api/v1/prewarm/jobs/${job.id}`) return json(job, 200, '"1"');
  if (url.startsWith("/api/v1/prewarm/runs?")) return json({ items: [run] });
  if (url === `/api/v1/prewarm/runs/${run.id}`) return json(run);
  if (url.startsWith(`/api/v1/prewarm/runs/${run.id}/items`))
    return json({
      items: [
        {
          seq: 3,
          name: "library/missing",
          version: "1.0",
          state: "failed",
          bytes: 0,
          attempts: 1,
          error: "not_found",
        },
      ],
    });
  if (url === `/api/v1/prewarm/runs/${run.id}/retry`)
    return json(
      { ...run, id: "00000000-0000-0000-0000-0000000000b2", state: "queued" },
      202,
    );
  return undefined;
};

it("lists prewarm jobs with their last run and shows why items failed", async () => {
  install(handlers);
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Cache prewarm/ }));
  expect(await screen.findByText("base images")).toBeTruthy();
  expect(screen.getByText(/2 warmed · 1 cached · 1 failed/)).toBeTruthy();
  expect(screen.getByText("Daily")).toBeTruthy();
  fireEvent.click(screen.getByText("base images"));
  const detail = await screen.findByRole("dialog", { name: "base images" });
  fireEvent.click(await within(detail).findByText("partial"));
  const runDialog = await screen.findByRole("dialog", {
    name: "Run · partial",
  });
  expect(
    await within(runDialog).findByText("library/missing 1.0"),
  ).toBeTruthy();
  expect(within(runDialog).getByText("Not found upstream")).toBeTruthy();
  fireEvent.click(
    within(runDialog).getByRole("button", { name: "Retry failed items" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(writes("POST")[0].url).toBe(`/api/v1/prewarm/runs/${run.id}/retry`);
});

it("creates an image prewarm job with a schedule", async () => {
  install(handlers);
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Cache prewarm/ }));
  fireEvent.click(await screen.findByRole("button", { name: "New job" }));
  const dialog = await screen.findByRole("dialog", { name: "New prewarm job" });
  fireEvent.change(within(dialog).getByLabelText("Name"), {
    target: { value: "new job" },
  });
  fireEvent.change(within(dialog).getByLabelText("Images"), {
    target: { value: "library/alpine:3.20\nlibrary/busybox" },
  });
  fireEvent.change(within(dialog).getByLabelText("Schedule"), {
    target: { value: "86400" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create job" }));
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  const body = JSON.parse(writes("POST")[0].init.body as string);
  expect(body).toMatchObject({
    feed_id: ids.feed,
    name: "new job",
    source: {
      kind: "images",
      images: ["library/alpine:3.20", "library/busybox"],
    },
    interval_seconds: 86400,
    run_now: true,
  });
  expect(
    new Headers(writes("POST")[0].init.headers).get("Idempotency-Key"),
  ).toBeTruthy();
});

it("says so when the server has no prewarm support", async () => {
  install();
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(await screen.findByRole("button", { name: /Cache prewarm/ }));
  expect(await screen.findByText(/Cache prewarming/)).toBeTruthy();
});
