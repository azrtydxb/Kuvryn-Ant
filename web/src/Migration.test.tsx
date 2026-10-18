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
const open = () =>
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name: "Import & migrate" }),
  );
const report = (dry: boolean) => ({
  dry_run: dry,
  summary: { feed: { created: 1 }, asset: { conflict: 1 } },
  items: [
    { kind: "feed", name: "libs", action: "created" },
    {
      kind: "asset",
      name: "hosted:a/x",
      action: "conflict",
      reason: "target has different content",
    },
  ],
  unmapped: [
    { kind: "credential", name: "proxy", reason: "provision the secret" },
  ],
  integrity_failures: [{ digest: "ab".repeat(32), reason: "digest mismatch" }],
});
const doc = {
  format_version: 1,
  sources: [],
  feeds: [],
  components: [],
};

it("requires a dry run before applying an import and shows the mapping report", async () => {
  install((url, init) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    if (url === "/api/v1/transfer/import" && init.method === "POST") {
      const body = JSON.parse(String(init.body));
      return json(report(body.dry_run));
    }
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  open();
  await screen.findByText("No transfers yet.");
  const apply = screen.getByRole("button", { name: "Apply import" });
  expect(apply).toHaveProperty("disabled", true);
  fireEvent.change(screen.getByLabelText("Export document"), {
    target: { value: JSON.stringify(doc) },
  });
  fireEvent.click(screen.getAllByRole("button", { name: "Dry run" })[0]);
  await screen.findByText("Dry run — nothing was changed");
  await screen.findByText("target has different content");
  await screen.findByText("provision the secret");
  await screen.findByText("digest mismatch");
  const post = calls.find((c) => c.url === "/api/v1/transfer/import")!;
  expect(JSON.parse(String(post.init.body))).toEqual({
    document: doc,
    dry_run: true,
  });
  await waitFor(() => expect(apply).toHaveProperty("disabled", false));
  // Editing the document invalidates the dry run.
  fireEvent.change(screen.getByLabelText("Export document"), {
    target: { value: JSON.stringify({ ...doc, feeds: [] }) + " " },
  });
  await waitFor(() => expect(apply).toHaveProperty("disabled", true));
  fireEvent.change(screen.getByLabelText("Export document"), {
    target: { value: JSON.stringify(doc) },
  });
  fireEvent.click(apply);
  await waitFor(() =>
    expect(
      calls
        .filter((c) => c.url === "/api/v1/transfer/import")
        .map((c) => JSON.parse(String(c.init.body)).dry_run),
    ).toEqual([true, false]),
  );
});

it("sends Nexus credentials only in the request body and compares coordinates", async () => {
  install((url, init) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    if (url === "/api/v1/migrations/nexus" && init.method === "POST")
      return json({
        ...report(true),
        manager: "nexus",
        repositories: [
          {
            name: "maven-public",
            type: "group",
            format: "maven2",
            mapped_to: "feed",
          },
        ],
      });
    if (url === "/api/v1/migrations/compare" && init.method === "POST")
      return json({
        manager: "nexus",
        counts: { differs: 1 },
        results: [
          {
            repository: "raw",
            path: "a.txt",
            feed: "raw",
            status: "differs",
            source: { found: true, sha256: "aa".repeat(32) },
            ant: { found: true, sha256: "bb".repeat(32), detail: "source s" },
          },
        ],
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  open();
  await screen.findByText("No transfers yet.");
  fireEvent.change(screen.getByLabelText("Nexus URL"), {
    target: { value: "https://nexus.example.com" },
  });
  fireEvent.change(screen.getByLabelText("Username"), {
    target: { value: "admin" },
  });
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: "s3cret" },
  });
  fireEvent.click(screen.getAllByRole("button", { name: "Dry run" })[1]);
  await screen.findByText("maven-public");
  const nexus = calls.find((c) => c.url === "/api/v1/migrations/nexus")!;
  expect(JSON.parse(String(nexus.init.body))).toMatchObject({
    url: "https://nexus.example.com",
    username: "admin",
    password: "s3cret",
    dry_run: true,
  });
  expect(nexus.url).not.toContain("s3cret");
  fireEvent.change(screen.getByLabelText("Coordinates"), {
    target: { value: "raw a.txt" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Compare" }));
  await screen.findByText("differs");
  await screen.findByText("raw/a.txt");
});
