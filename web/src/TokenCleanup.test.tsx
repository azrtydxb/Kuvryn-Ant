// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from "@testing-library/react";
import App from "./App";
import { AuditPage } from "./Audit";
import { Identity } from "./Identity";
import { Client } from "./api";
import { ids, json, mockAPI, signInWithToken, type Call } from "./testkit";

let calls: Call[];
function install(...args: Parameters<typeof mockAPI>) {
  const api = mockAPI(...args);
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
beforeEach(() => {
  localStorage.clear();
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
const writes = (method: string) =>
  calls.filter((c) => c.init.method === method);
const tab = (name: string) =>
  fireEvent.click(screen.getByRole("tab", { name: new RegExp(name) }));
const DAY = 86400000;
const iso = (offsetDays: number) =>
  new Date(Date.now() + offsetDays * DAY).toISOString();

const token = (id: string, state: string, expires: number, extra = {}) => ({
  id,
  principal_id: ids.service,
  scopes: [{ actions: ["read"] }],
  status: state,
  state,
  created_at: "2026-01-01T00:00:00Z",
  expires_at: iso(expires),
  ...extra,
});
const A = "aaaaaaaa-0000-0000-0000-000000000001";
const SOON = "bbbbbbbb-0000-0000-0000-000000000002";
const EXP = "cccccccc-0000-0000-0000-000000000003";
const REV = "dddddddd-0000-0000-0000-000000000004";
const counts = { active: 2, expired: 1, revoked: 1 };

// Behaves like the server: filters by the requested state.
function tokenServer(url: string) {
  if (!url.startsWith("/api/v1/tokens?")) return undefined;
  const state = new URL(url, "http://x").searchParams.get("state");
  const all = [
    token(A, "active", 60),
    token(SOON, "active", 3),
    token(EXP, "expired", -4),
    token(REV, "revoked", 30, { revoked_at: iso(-1) }),
  ];
  const items =
    state === "all" ? all : all.filter((t) => t.state === (state ?? "active"));
  return json({ items, page: { has_more: false }, counts });
}
const tokenURLs = () =>
  calls.map((c) => c.url).filter((u) => u.startsWith("/api/v1/tokens?"));

it("hides expired and revoked service tokens by default and shows a hidden count", async () => {
  install((url) => tokenServer(url));
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText("Expires in 3 days");
  expect(tokenURLs().at(-1)).toContain("state=active");
  expect(screen.queryByText("Expired")).toBeNull();
  expect(screen.queryByText("Revoked")).toBeNull();
  expect((await screen.findByTestId("hidden-tokens")).textContent).toBe(
    "1 expired and 1 revoked hidden",
  );
  // The warning window is 7 days.
  expect(screen.getByText(/1 token expire within 7 days/)).toBeTruthy();
});

it("the toggle reveals expired and revoked tokens with muted rows and badges, and is remembered", async () => {
  install((url) => tokenServer(url));
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText("Expires in 3 days");
  fireEvent.click(screen.getByLabelText("Show expired and revoked tokens"));
  await screen.findByText("Expired");
  expect(screen.getByText("Revoked")).toBeTruthy();
  expect(tokenURLs().at(-1)).toContain("state=all");
  expect(screen.queryByTestId("hidden-tokens")).toBeNull();
  // Dead rows are muted; active rows are not.
  const expiredRow = screen.getByText("Expired").closest("tr")!;
  expect(expiredRow.querySelectorAll(".tok-dead").length).toBeGreaterThan(0);
  const activeRow = screen.getByText("Active").closest("tr")!;
  expect(activeRow.querySelectorAll(".tok-dead").length).toBe(0);
  expect(localStorage.getItem("ant.tokens.showInactive")).toBe("1");
  // A new mount starts with the remembered choice.
  cleanup();
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText("Expired");
  expect(tokenURLs().at(-1)).toContain("state=all");
});

it("survives unavailable browser storage", async () => {
  vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
    throw new Error("denied");
  });
  vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
    throw new Error("denied");
  });
  install((url) => tokenServer(url));
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText("Expires in 3 days");
  fireEvent.click(screen.getByLabelText("Show expired and revoked tokens"));
  await screen.findByText("Expired");
});

const pat = (id: string, name: string, state: string, expires: number) => ({
  id,
  name,
  scopes: [{ actions: ["read"] }],
  created_at: "2026-01-01T00:00:00Z",
  expires_at: iso(expires),
  state,
});
const settings = {
  revision: 4,
  items: [
    {
      key: "token_cleanup_after_days",
      value: "30",
      source: "default",
      read_only: false,
      enforced: true,
      description: "d",
    },
  ],
};

async function openTokens() {
  fireEvent.click(
    screen
      .getAllByRole("button", { name: "Settings" })
      .find((b) => b.className.includes("az-nav__item"))!,
  );
  fireEvent.click(await screen.findByRole("button", { name: "Tokens" }));
}

it("personal tokens hide inactive ones and delete only the user's own expired tokens after confirmation", async () => {
  install((url, init) => {
    if (url === "/api/v1/auth/personal-access-tokens/cleanup")
      return json({ deleted: 2 });
    if (url.startsWith("/api/v1/auth/personal-access-tokens")) {
      const state = new URL(url, "http://x").searchParams.get("state");
      const items = [pat("p1", "laptop", "active", 40)];
      return json({
        items:
          state === "all"
            ? [...items, pat("p2", "old-ci", "expired", -9)]
            : items,
        counts: { active: 1, expired: 1, revoked: 1 },
      });
    }
    if (url.startsWith("/api/v1/tokens")) return json({ items: [] });
    return init.method ? undefined : undefined;
  });
  render(<App />);
  await signInWithToken();
  await openTokens();
  await screen.findByText("laptop");
  expect(screen.queryByText("old-ci")).toBeNull();
  expect(
    calls.some(
      (c) => c.url === "/api/v1/auth/personal-access-tokens?state=active",
    ),
  ).toBe(true);
  expect((await screen.findAllByTestId("hidden-tokens"))[0].textContent).toBe(
    "1 expired and 1 revoked hidden",
  );
  fireEvent.click(screen.getByRole("button", { name: /Delete expired/ }));
  const dialog = await screen.findByRole("dialog", {
    name: "Delete expired tokens",
  });
  expect(dialog.textContent).toContain("Delete your 2 expired or revoked");
  expect(writes("POST")).toHaveLength(0);
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Delete expired tokens" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(writes("POST")[0].url).toBe(
    "/api/v1/auth/personal-access-tokens/cleanup",
  );
});

it("admin cleanup previews the count, asks for confirmation, then deletes", async () => {
  install((url, init) => {
    if (url === "/api/v1/settings" && init.method === "PUT")
      return json({ ...settings, revision: 5 });
    if (url === "/api/v1/settings") return json(settings, 200, '"4"');
    if (url === "/api/v1/tokens/cleanup") {
      const dry = JSON.parse(init.body as string).dry_run;
      return json({
        dry_run: dry,
        after_days: 30,
        cutoff: iso(-30),
        service_tokens: 10,
        personal_access_tokens: 4,
        total: 14,
        has_more: false,
      });
    }
    if (url.startsWith("/api/v1/auth/personal-access-tokens"))
      return json({ items: [], counts: { active: 0, expired: 0, revoked: 0 } });
    return tokenServer(url);
  });
  render(<App />);
  await signInWithToken();
  await openTokens();
  const cleanup = await screen.findByTestId("token-cleanup");
  expect(
    (
      within(cleanup).getByLabelText(
        /Delete expired or revoked tokens after/,
      ) as HTMLInputElement
    ).value,
  ).toBe("30");
  fireEvent.click(
    within(cleanup).getByRole("button", { name: "Clean up now" }),
  );
  const dialog = await screen.findByRole("dialog", { name: "Clean up tokens" });
  expect(dialog.textContent).toContain(
    "Delete 14 tokens expired or revoked more than 30 days ago?",
  );
  // The preview is a dry run: nothing is deleted until confirmed.
  expect(
    writes("POST").map((c) => JSON.parse(c.init.body as string).dry_run),
  ).toEqual([true]);
  fireEvent.click(within(dialog).getByRole("button", { name: "Delete 14" }));
  await waitFor(() => expect(writes("POST")).toHaveLength(2));
  expect(JSON.parse(writes("POST")[1].init.body as string)).toEqual({
    dry_run: false,
  });
  expect(
    new Headers(writes("POST")[1].init.headers).get("Idempotency-Key"),
  ).toBeTruthy();
});

it("saves the retention days with the settings revision and merges only that field", async () => {
  install((url, init) => {
    if (url === "/api/v1/settings" && init.method === "PUT")
      return json({ ...settings, revision: 5 });
    if (url === "/api/v1/settings") return json(settings, 200, '"4"');
    if (url.startsWith("/api/v1/auth/personal-access-tokens"))
      return json({ items: [], counts: { active: 0, expired: 0, revoked: 0 } });
    return tokenServer(url);
  });
  render(<App />);
  await signInWithToken();
  await openTokens();
  const section = await screen.findByTestId("token-cleanup");
  const input = within(section).getByLabelText(
    /Delete expired or revoked tokens after/,
  );
  const save = within(section).getByRole("button", { name: "Save" });
  expect((save as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(input, { target: { value: "-3" } });
  expect((save as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(input, { target: { value: "0" } });
  fireEvent.click(save);
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const put = writes("PUT")[0];
  expect(JSON.parse(put.init.body as string)).toEqual({
    token_cleanup_after_days: 0,
  });
  expect(new Headers(put.init.headers).get("If-Match")).toBe('"4"');
});

it("env-provided retention is read-only and an empty preview offers no deletion", async () => {
  install((url, init) => {
    if (url === "/api/v1/settings")
      return json({
        ...settings,
        items: [
          { ...settings.items[0], value: "7", source: "env", read_only: true },
        ],
      });
    if (url === "/api/v1/tokens/cleanup" && init.method === "POST")
      return json({
        dry_run: true,
        after_days: 7,
        cutoff: iso(-7),
        service_tokens: 0,
        personal_access_tokens: 0,
        total: 0,
        has_more: false,
      });
    if (url.startsWith("/api/v1/auth/personal-access-tokens"))
      return json({ items: [], counts: { active: 0, expired: 0, revoked: 0 } });
    return tokenServer(url);
  });
  render(<App />);
  await signInWithToken();
  await openTokens();
  const section = await screen.findByTestId("token-cleanup");
  expect(
    (
      within(section).getByLabelText(
        /Delete expired or revoked tokens after/,
      ) as HTMLInputElement
    ).disabled,
  ).toBe(true);
  expect(section.textContent).toContain("ANT_TOKEN_CLEANUP_AFTER_DAYS");
  fireEvent.click(
    within(section).getByRole("button", { name: "Clean up now" }),
  );
  const dialog = await screen.findByRole("dialog", { name: "Clean up tokens" });
  expect(within(dialog).queryByRole("button", { name: /^Delete/ })).toBeNull();
  expect(dialog.textContent).toContain(
    "No tokens have been expired or revoked",
  );
});

it("audit rows keep an opaque token id and show a deleted token once cleanup removed it", async () => {
  install((url) => {
    if (url.startsWith("/api/v1/audit?"))
      return json({
        items: [
          {
            id: 1,
            created_at: iso(-1),
            action: "token.issue",
            resource_id: "eeeeeeee-0000-0000-0000-000000000009",
            result: "success",
            principal_id: ids.service,
          },
          {
            id: 2,
            created_at: iso(-1),
            action: "token.cleanup",
            result: "success",
          },
        ],
        page: { has_more: false },
      });
    return tokenServer(url);
  });
  render(<AuditPage client={new Client("token")} />);
  await screen.findByText("deleted token");
  expect(screen.getByText("system")).toBeTruthy();
});
