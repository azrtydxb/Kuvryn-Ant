// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
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
const writes = (m: string) => calls.filter((c) => c.init.method === m);

const item = (key: string, value: string, extra = {}) => ({
  key,
  value,
  source: "default",
  read_only: false,
  enforced: false,
  description: `${key} description`,
  ...extra,
});
const settings = {
  revision: 2,
  items: [
    item("session_max_age_seconds", "28800"),
    item("session_idle_timeout_seconds", "1800"),
    item("default_anonymous_read", "false"),
    item("public_url", "https://ant.example", {
      source: "env",
      read_only: true,
      enforced: true,
    }),
  ],
};

async function openSettings(tab: string) {
  fireEvent.click(
    screen
      .getAllByRole("button", { name: "Settings" })
      .find((b) => b.className.includes("az-nav__item"))!,
  );
  fireEvent.click(await screen.findByRole("button", { name: tab }));
}

it("general settings: env values are read-only, unenforced are flagged, save is revision-guarded", async () => {
  install((url, init) => {
    if (url === "/api/v1/settings" && init.method === "PUT")
      return json({ ...settings, revision: 3 });
    if (url === "/api/v1/settings") return json(settings);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  await openSettings("General");
  await screen.findByText("public_url");
  expect(screen.getByText("https://ant.example")).toBeTruthy();
  expect(screen.getAllByText("read-only").length).toBe(1);
  expect(screen.getAllByText("recorded only").length).toBe(3);
  const save = screen.getByRole("button", { name: "Save" });
  expect((save as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(screen.getByLabelText("Anonymous reads for new feeds"));
  expect((save as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(save);
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const put = writes("PUT")[0];
  const headers = new Headers(put.init.headers);
  expect(headers.get("If-Match")).toBe('"2"');
  expect(headers.get("Idempotency-Key")).toBeTruthy();
  expect(JSON.parse(put.init.body as string)).toEqual({
    session_max_age_seconds: 28800,
    session_idle_timeout_seconds: 1800,
    default_anonymous_read: true,
  });
});

it("save omits environment-backed keys the server would reject", async () => {
  const locked = {
    ...settings,
    items: [
      item("session_max_age_seconds", "3600", {
        source: "env",
        read_only: true,
        enforced: true,
      }),
      item("session_idle_timeout_seconds", "1800"),
      item("default_anonymous_read", "false"),
    ],
  };
  install((url, init) => {
    if (url === "/api/v1/settings" && init.method === "PUT")
      return json(locked);
    if (url === "/api/v1/settings") return json(locked);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  await openSettings("General");
  fireEvent.click(
    await screen.findByLabelText("Anonymous reads for new feeds"),
  );
  fireEvent.click(screen.getByRole("button", { name: "Save" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const body = JSON.parse(writes("PUT")[0].init.body as string);
  expect(body).not.toHaveProperty("session_max_age_seconds");
  expect(body.default_anonymous_read).toBe(true);
});

const pat = {
  id: "00000000-0000-0000-0000-0000000000a1",
  name: "laptop",
  scopes: [{ actions: ["read"] }],
  created_at: "2026-01-01T00:00:00Z",
  expires_at: "2099-01-01T00:00:00Z",
};
const svc = {
  id: "00000000-0000-0000-0000-0000000000b1",
  principal_id: "00000000-0000-0000-0000-000000000004",
  scopes: [{ actions: ["read"] }],
  status: "active",
  created_at: "2026-01-01T00:00:00Z",
  expires_at: "2099-01-01T00:00:00Z",
};

it("tokens page separates personal tokens from service-account tokens and sends scopes/expiry on create", async () => {
  install((url, init) => {
    if (url === "/api/v1/auth/personal-access-tokens" && init.method === "POST")
      return json({ ...pat, token: "secret-token-value" }, 201);
    if (url === "/api/v1/auth/personal-access-tokens")
      return json({ items: [pat] });
    if (url.startsWith("/api/v1/tokens")) return json({ items: [svc] });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  await openSettings("Tokens");
  await screen.findByText("Personal access tokens");
  await screen.findByText("Service account tokens");
  await screen.findByText("laptop");
  await screen.findByText("automation");
  fireEvent.click(screen.getByRole("button", { name: "Create token" }));
  fireEvent.change(await screen.findByLabelText("Name"), {
    target: { value: "ci" },
  });
  fireEvent.change(screen.getByLabelText("Namespace (optional)"), {
    target: { value: "team" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create token once" }));
  await screen.findByText(/Shown once/);
  const post = writes("POST")[0];
  expect(JSON.parse(post.init.body as string)).toEqual({
    name: "ci",
    scopes: [{ actions: ["read"], namespace: "team" }],
    expires_in_days: 90,
  });
  expect(new Headers(post.init.headers).get("Idempotency-Key")).toBeTruthy();
});

it("a service-token session sees an explanation instead of a raw 403 for personal tokens", async () => {
  install((url) => {
    if (url === "/api/v1/auth/personal-access-tokens")
      return json({ code: "forbidden" }, 403);
    if (url.startsWith("/api/v1/tokens")) return json({ items: [] });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  await openSettings("Tokens");
  await screen.findByText(/Sign in as a person/);
  await screen.findByText("Service account tokens");
});
