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
const tab = (name: string) =>
  fireEvent.click(screen.getByRole("tab", { name: new RegExp(name) }));
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
  tab("History");
  await screen.findByText("No transfers yet.");
  tab("Import");
  const apply = screen.getByRole("button", { name: "Apply import" });
  expect(apply).toHaveProperty("disabled", true);
  fireEvent.change(screen.getByLabelText("Export document"), {
    target: { value: JSON.stringify(doc) },
  });
  fireEvent.click(screen.getByRole("button", { name: "Dry run" }));
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

const group = (name: string) =>
  within(screen.getByRole("group", { name: `${name} migration` }));
const nexusRepos = {
  ...report(true),
  manager: "nexus",
  repositories: [
    {
      name: "maven-public",
      type: "group",
      format: "maven2",
      ant_format: "maven",
      mapped_to: "feed",
    },
  ],
};

it("sends Nexus credentials only in the request body and compares coordinates", async () => {
  install((url, init) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    if (url === "/api/v1/migrations/nexus" && init.method === "POST")
      return json(nexusRepos);
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
  tab("Migrate");
  const nexus = group("Nexus");
  fireEvent.change(nexus.getByLabelText("Nexus URL"), {
    target: { value: "https://nexus.example.com" },
  });
  fireEvent.change(nexus.getByLabelText("Username"), {
    target: { value: "admin" },
  });
  fireEvent.change(nexus.getByLabelText("Password"), {
    target: { value: "s3cret" },
  });
  fireEvent.click(nexus.getByRole("button", { name: "Discover repositories" }));
  await nexus.findByText("maven-public");
  const post = calls.find((c) => c.url === "/api/v1/migrations/nexus")!;
  expect(JSON.parse(String(post.init.body))).toMatchObject({
    url: "https://nexus.example.com",
    username: "admin",
    password: "s3cret",
    dry_run: true,
  });
  expect(post.url).not.toContain("s3cret");
  // The coordinates form lives in a dialog, not on the page.
  expect(screen.queryByLabelText("Coordinates")).toBeNull();
  fireEvent.click(nexus.getByRole("button", { name: "Compare…" }));
  fireEvent.change(screen.getByLabelText("Coordinates"), {
    target: { value: "raw a.txt" },
  });
  fireEvent.click(
    within(screen.getByRole("dialog")).getByRole("button", {
      name: "Compare",
    }),
  );
  await screen.findByText("differs");
  await screen.findByText("raw/a.txt");
  expect(
    JSON.parse(
      String(
        calls.find((c) => c.url === "/api/v1/migrations/compare")!.init.body,
      ),
    ),
  ).toMatchObject({ manager: "nexus", url: "https://nexus.example.com" });
});

it("splits the page into Export, Import, Migrate and History tabs and keeps work across switches", async () => {
  install((url) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  open();
  const tabs = screen.getAllByRole("tab").map((t) => t.textContent);
  expect(tabs.map((t) => t?.replace(/\d+$/, ""))).toEqual([
    "Export",
    "Import",
    "Migrate",
    "History",
  ]);
  expect(
    screen.getByRole("button", { name: "Export definitions" }),
  ).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Apply import" })).toBeNull();
  tab("Import");
  fireEvent.change(screen.getByLabelText("Export document"), {
    target: { value: "{}" },
  });
  tab("History");
  expect(
    screen.getByLabelText("Export document").closest("[hidden]"),
  ).not.toBeNull();
  tab("Import");
  expect(
    (screen.getByLabelText("Export document") as HTMLTextAreaElement).value,
  ).toBe("{}");
});

it("switches between Nexus and Artifactory on one page and keeps each source's input", async () => {
  install((url) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    return undefined;
  });
  const { container } = render(<App />);
  await signInWithToken();
  open();
  // Only the migrate tab uses the fixed-height, no-page-scroll layout.
  expect(container.querySelector(".mig--fit")).toBeNull();
  tab("Migrate");
  expect(container.querySelector(".mig--fit")).not.toBeNull();
  expect(
    screen.getByRole("radio", { name: "Nexus" }).getAttribute("aria-checked"),
  ).toBe("true");
  fireEvent.change(group("Nexus").getByLabelText("Nexus URL"), {
    target: { value: "https://nexus.example.com" },
  });
  expect(
    screen.queryByRole("group", { name: "Artifactory migration" }),
  ).toBeNull();
  fireEvent.click(screen.getByRole("radio", { name: "Artifactory" }));
  expect(screen.queryByRole("group", { name: "Nexus migration" })).toBeNull();
  const art = group("Artifactory");
  // Artifactory defaults to an access token; user + key is one click away.
  expect(art.getByLabelText("Access token")).toBeTruthy();
  fireEvent.click(art.getByRole("radio", { name: "User" }));
  expect(art.getByLabelText("Password or API key")).toBeTruthy();
  // The connection, repository table and action bar share one screen.
  const panel = screen.getByRole("group", { name: "Artifactory migration" });
  expect(panel.querySelector(".mig-conn")).not.toBeNull();
  expect(panel.querySelector(".mig-main .mig-bar")).not.toBeNull();
  expect(art.getByRole("button", { name: "Dry run" })).toHaveProperty(
    "disabled",
    true,
  );
  fireEvent.click(screen.getByRole("radio", { name: "Nexus" }));
  expect(
    (group("Nexus").getByLabelText("Nexus URL") as HTMLInputElement).value,
  ).toBe("https://nexus.example.com");
});

const artifactory = {
  ...report(true),
  manager: "artifactory",
  summary: { source: { created: 2 }, feed: { created: 3 } },
  items: [{ kind: "feed", name: "libs-all", action: "created" }],
  unmapped: [
    {
      kind: "repository",
      name: "bower-local",
      reason: "format bower has no Ant equivalent",
    },
  ],
  integrity_failures: [],
  repositories: [
    {
      name: "libs-local",
      type: "hosted",
      format: "maven",
      ant_format: "maven",
      mapped_to: "local source + feed libs-local",
      assets: 120,
      bytes: 2048,
    },
    {
      name: "central",
      type: "proxy",
      format: "maven",
      ant_format: "maven",
      mapped_to: "remote source + feed central",
    },
    {
      name: "libs-all",
      type: "group",
      format: "maven",
      ant_format: "maven",
      mapped_to: "feed",
    },
    {
      name: "bower-local",
      type: "hosted",
      format: "bower",
      mapped_to: "unmapped",
    },
  ],
};

it("discovers Artifactory repositories, dry-runs the selection in a drawer and applies only a current dry run", async () => {
  install((url, init) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    if (url === "/api/v1/migrations/artifactory" && init.method === "POST") {
      const body = JSON.parse(String(init.body));
      return json({
        ...artifactory,
        dry_run: body.dry_run,
        summary: body.dry_run ? artifactory.summary : { feed: { created: 3 } },
      });
    }
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  open();
  tab("Migrate");
  fireEvent.click(screen.getByRole("radio", { name: "Artifactory" }));
  const art = group("Artifactory");
  fireEvent.change(art.getByLabelText("Artifactory URL"), {
    target: { value: "https://acme.jfrog.io/artifactory" },
  });
  fireEvent.change(art.getByLabelText("Access token"), {
    target: { value: "tok-secret" },
  });
  fireEvent.click(art.getByRole("button", { name: "Discover repositories" }));
  await art.findByText("libs-local");
  const discover = calls.find(
    (c) => c.url === "/api/v1/migrations/artifactory",
  )!;
  expect(JSON.parse(String(discover.init.body))).toEqual({
    url: "https://acme.jfrog.io/artifactory",
    token: "tok-secret",
    repositories: [],
    count_assets: false,
    dry_run: true,
  });
  expect(discover.url).not.toContain("tok-secret");
  // Artifactory's own vocabulary and sizes; unmapped repositories cannot be chosen.
  const row = (n: string) => within(art.getByText(n).closest("tr")!);
  expect(row("libs-local").getByText("local")).toBeTruthy();
  expect(row("central").getByText("remote")).toBeTruthy();
  expect(row("libs-all").getByText("virtual")).toBeTruthy();
  expect(art.getByText("2.0 KB")).toBeTruthy();
  expect(art.getByText("not mapped")).toBeTruthy();
  expect(art.getByLabelText("Include bower-local")).toHaveProperty(
    "disabled",
    true,
  );
  expect(art.getByText("1 note")).toBeTruthy();
  expect(
    screen
      .getByRole("group", { name: "Artifactory migration" })
      .querySelector(".mig-bar__summary")!.textContent,
  ).toContain("3 of 4 selected");
  const migrate = art.getByRole("button", { name: "Migrate" });
  expect(migrate).toHaveProperty("disabled", true);
  // Leave one repository out and run the dry run for the selection.
  fireEvent.click(art.getByLabelText("Include central"));
  fireEvent.click(art.getByRole("button", { name: "Dry run" }));
  const drawer = await screen.findByRole("dialog", { name: "Dry run report" });
  within(drawer).getByText("Dry run — nothing was changed");
  within(drawer).getByText("libs-all");
  within(drawer).getByText("format bower has no Ant equivalent");
  const dry = JSON.parse(
    String(
      calls.filter((c) => c.url === "/api/v1/migrations/artifactory")[1].init
        .body,
    ),
  );
  expect(dry).toMatchObject({
    repositories: ["libs-local", "libs-all"],
    dry_run: true,
  });
  fireEvent.click(within(drawer).getByRole("button", { name: "Close" }));
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  await waitFor(() => expect(migrate).toHaveProperty("disabled", false));
  // Changing the selection invalidates the dry run.
  fireEvent.click(art.getByLabelText("Include libs-all"));
  expect(migrate).toHaveProperty("disabled", true);
  fireEvent.click(art.getByLabelText("Include libs-all"));
  await waitFor(() => expect(migrate).toHaveProperty("disabled", false));
  fireEvent.click(migrate);
  await screen.findByRole("dialog", { name: "Migration applied" });
  const apply = JSON.parse(
    String(
      calls.filter((c) => c.url === "/api/v1/migrations/artifactory")[2].init
        .body,
    ),
  );
  expect(apply).toMatchObject({
    repositories: ["libs-local", "libs-all"],
    dry_run: false,
  });
});

it("keeps a long repository list inside its own scroll area", async () => {
  const many = Array.from({ length: 40 }, (_, i) => ({
    name: `repo-${i}`,
    type: "hosted",
    format: "raw",
    ant_format: "raw",
    mapped_to: "local source",
    bytes: 1024,
  }));
  install((url, init) => {
    if (url === "/api/v1/transfer/runs") return json({ items: [] });
    if (url === "/api/v1/migrations/nexus" && init.method === "POST")
      return json({ ...nexusRepos, repositories: many });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  open();
  tab("Migrate");
  const nexus = group("Nexus");
  fireEvent.change(nexus.getByLabelText("Nexus URL"), {
    target: { value: "https://nexus.example.com" },
  });
  fireEvent.click(nexus.getByRole("button", { name: "Discover repositories" }));
  await nexus.findByText("repo-39");
  const panel = screen.getByRole("group", { name: "Nexus migration" });
  // Rows live in the table scroller; the action bar is a sibling, never inside it.
  const scroller = panel.querySelector(".mig-table")!;
  expect(scroller.querySelectorAll("tbody tr")).toHaveLength(40);
  expect(scroller.contains(panel.querySelector(".mig-bar"))).toBe(false);
  const summary = () => panel.querySelector(".mig-bar__summary")!.textContent;
  expect(summary()).toContain("40 of 40 selected");
  fireEvent.change(nexus.getByLabelText("Filter repositories"), {
    target: { value: "repo-3" },
  });
  expect(scroller.querySelectorAll("tbody tr")).toHaveLength(11);
  fireEvent.click(nexus.getByLabelText("Select all repositories"));
  expect(summary()).toContain("0 of 40 selected");
});
