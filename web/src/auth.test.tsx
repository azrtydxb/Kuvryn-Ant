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
import { Client } from "./api";
import { AUTH_TIMEOUT_MS, getProviders, parseSession } from "./auth";
import { signOutViaMenu } from "./testkit";
const csrf = "a".repeat(43),
  prelogin = "b".repeat(43);
const session = {
  csrf_token: csrf,
  session: {
    PrincipalID: "user-1",
    InstanceID: "instance-1",
    ExpiresAt: new Date(Date.now() + 3600000).toISOString(),
  },
};
const json = (v: unknown, status = 200) =>
  new Response(JSON.stringify(v), { status });
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
it("resumes a cookie session, adds CSRF to mutations, and signs out server-side", async () => {
  const calls: { url: string; init: RequestInit }[] = [];
  const storage = vi.spyOn(Storage.prototype, "setItem");
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      if (url === "/auth/session") return json(session);
      if (url === "/auth/logout") return new Response(null, { status: 204 });
      if (url === "/auth/providers") return json({ providers: [] });
      if (url.includes("/feeds?"))
        return json({ items: [], page: { has_more: false } });
      if (url.includes("/sources?"))
        return json({ items: [], page: { has_more: false } });
      if (url === "/api/v1/feeds" && init.method === "POST")
        return json({
          id: "feed-1",
          slug: "release",
          format: "oci",
          revision: 1,
        });
      if (url === "/api/v1/status")
        return json({ name: "kuvryn-ant", api_version: "v1" });
      return json({}, 404);
    }),
  );
  render(<App />);
  await screen.findByRole("button", { name: /^Account menu/ });
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name: "Feeds" }),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Create feed" }));
  const dialog = screen.getByRole("dialog", { name: "Create feed" });
  fireEvent.change(within(dialog).getByLabelText("Feed slug"), {
    target: { value: "release" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Create feed" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "POST")).toBe(true),
  );
  const mutation = calls.find((c) => c.init.method === "POST")!;
  expect(mutation.init.credentials).toBe("same-origin");
  expect(new Headers(mutation.init.headers).get("X-CSRF-Token")).toBe(csrf);
  expect(new Headers(mutation.init.headers).has("Authorization")).toBe(false);
  await signOutViaMenu();
  await screen.findByRole("heading", { name: "Sign in" });
  const signout = calls.find((c) => c.url === "/auth/logout")!;
  expect(signout.init.method).toBe("POST");
  expect(new Headers(signout.init.headers).get("X-CSRF-Token")).toBe(csrf);
  expect(storage).not.toHaveBeenCalled();
  expect(document.body.textContent).not.toContain(csrf);
});
it("shows logout failure after clearing local protected views and can retry revocation", async () => {
  let attempt = 0;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/auth/session") return json(session);
      if (url === "/auth/providers") return json({ providers: [] });
      if (url === "/auth/logout")
        return ++attempt === 1
          ? new Response(null, { status: 503 })
          : new Response(null, { status: 204 });
      return url === "/api/v1/status"
        ? json({ name: "kuvryn-ant", api_version: "v1" })
        : json({ code: "not_found" }, 404);
    }),
  );
  render(<App />);
  await signOutViaMenu();
  await screen.findByRole("button", { name: "Retry server sign-out" });
  expect(screen.queryByRole("button", { name: /^Account menu/ })).toBeNull();
  fireEvent.click(
    screen.getByRole("button", { name: "Retry server sign-out" }),
  );
  await waitFor(() =>
    expect(
      screen.queryByRole("button", { name: "Retry server sign-out" }),
    ).toBeNull(),
  );
  expect(attempt).toBe(2);
});
it("discovers provider links and clears LDAP password before acquiring prelogin CSRF", async () => {
  let loggedIn = false;
  let release: (r: Response) => void = () => {};
  const calls: { url: string; init: RequestInit }[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      if (url === "/auth/session")
        return loggedIn ? json(session) : new Response(null, { status: 401 });
      if (url === "/auth/providers")
        return json({ providers: ["oidc", "saml", "ldap"] });
      if (url === "/auth/ldap/login" && init.method === "POST") {
        loggedIn = true;
        return json(session);
      }
      if (url === "/auth/ldap/login")
        return new Promise<Response>((resolve) => {
          release = resolve;
        });
      return json({ name: "kuvryn-ant", api_version: "v1" });
    }),
  );
  render(<App />);
  expect(
    (
      await screen.findByRole("link", { name: "Sign in with OIDC" })
    ).getAttribute("href"),
  ).toBe("/auth/oidc/login");
  expect(
    screen
      .getByRole("link", { name: "Sign in with SAML" })
      .getAttribute("href"),
  ).toBe("/auth/saml/login");
  fireEvent.change(screen.getByLabelText("LDAP username"), {
    target: { value: "operator" },
  });
  fireEvent.change(screen.getByLabelText("LDAP password"), {
    target: { value: "private-password" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Sign in with LDAP" }));
  expect(
    (screen.getByLabelText("LDAP password") as HTMLInputElement).value,
  ).toBe("");
  release(json({ csrf_token: prelogin }));
  await screen.findByRole("button", { name: /^Account menu/ });
  const post = calls.find(
    (c) => c.url === "/auth/ldap/login" && c.init.method === "POST",
  )!;
  expect(new Headers(post.init.headers).get("X-CSRF-Token")).toBe(prelogin);
  expect(new Headers(post.init.headers).has("Origin")).toBe(false);
  expect(post.init.credentials).toBe("same-origin");
  expect(JSON.parse(post.init.body as string)).toEqual({
    username: "operator",
    password: "private-password",
  });
  expect(document.body.textContent).not.toContain("private-password");
});
it("does not advertise providers when discovery is unknown", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => new Response(null, { status: 404 })),
  );
  expect(await getProviders()).toBeNull();
  render(<App />);
  await screen.findByText(/Provider availability could not be determined/);
  expect(screen.queryByRole("link", { name: "Sign in with OIDC" })).toBeNull();
});
it("rejects malformed session CSRF values", () => {
  expect(() => parseSession({ ...session, csrf_token: "bad\nheader" })).toThrow(
    "invalid session",
  );
});
it("never adds replay keys to one-shot token issuance, including browser sessions", async () => {
  const fetcher = vi.fn().mockResolvedValue(
    json({
      id: "token",
      secret: "secret",
      expires_at: session.session.ExpiresAt,
    }),
  );
  vi.stubGlobal("fetch", fetcher);
  await Client.browser(csrf).call("issueServiceToken", {
    params: { id: "service" },
    body: {
      scopes: [{ actions: ["read"] }],
      expires_at: session.session.ExpiresAt,
    },
  });
  const init = fetcher.mock.calls[0][1];
  expect(new Headers(init.headers).has("Idempotency-Key")).toBe(false);
  expect(new Headers(init.headers).get("X-CSRF-Token")).toBe(csrf);
  expect(init.credentials).toBe("same-origin");
});

it("stops checking the session and offers sign-in when /auth/session hangs", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn((url: string, init: RequestInit) => {
      if (url === "/auth/providers")
        return Promise.resolve(json({ providers: [] }));
      return new Promise<Response>((_, reject) =>
        init.signal?.addEventListener("abort", () =>
          reject(init.signal?.reason),
        ),
      );
    }),
  );
  vi.useFakeTimers({ shouldAdvanceTime: true });
  render(<App />);
  await vi.advanceTimersByTimeAsync(AUTH_TIMEOUT_MS + 1);
  expect(await screen.findByText(/could not be reached/i)).toBeTruthy();
  expect(screen.queryByText(/Checking your browser session/)).toBeNull();
  expect(
    screen.getByRole("button", { name: /retry session check/i }),
  ).toBeTruthy();
  vi.useRealTimers();
});
it("continues a SAML sign-out at the identity provider via the server-staged page", async () => {
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/auth/session") return json(session);
      if (url === "/auth/providers") return json({ providers: [] });
      if (url === "/auth/logout")
        return json({ saml_logout: "/auth/saml/logout" });
      return url === "/api/v1/status"
        ? json({ name: "kuvryn-ant", api_version: "v1" })
        : json({ code: "not_found" }, 404);
    }),
  );
  render(<App />);
  await screen.findByRole("button", { name: /^Account menu/ });
  await signOutViaMenu();
  await waitFor(() => expect(assign).toHaveBeenCalledWith("/auth/saml/logout"));
});
it("never navigates to a logout target outside the auth routes", async () => {
  const assign = vi.fn();
  vi.stubGlobal("location", { ...window.location, assign });
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/auth/session") return json(session);
      if (url === "/auth/providers") return json({ providers: [] });
      if (url === "/auth/logout")
        return json({ saml_logout: "https://evil.example/" });
      return url === "/api/v1/status"
        ? json({ name: "kuvryn-ant", api_version: "v1" })
        : json({ code: "not_found" }, 404);
    }),
  );
  render(<App />);
  await screen.findByRole("button", { name: /^Account menu/ });
  await signOutViaMenu();
  await screen.findByRole("heading", { name: "Sign in" });
  expect(assign).not.toHaveBeenCalled();
});
for (const [label, target, navigates] of [
  ["follows", "https://auth.example.com/oidc/v1/end_session?client_id=c", true],
  ["refuses a non-https", "http://auth.example.com/end", false],
  ["refuses a javascript", "javascript:alert(1)", false],
] as const)
  it(`${label} OIDC end-session target after the local sign-out`, async () => {
    const assign = vi.fn();
    vi.stubGlobal("location", { ...window.location, assign });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (url: string) => {
        if (url === "/auth/session") return json(session);
        if (url === "/auth/providers") return json({ providers: [] });
        if (url === "/auth/logout") return json({ oidc_logout: target });
        return json({ code: "not_found" }, 404);
      }),
    );
    render(<App />);
    await screen.findByRole("button", { name: /^Account menu/ });
    await signOutViaMenu();
    await screen.findByRole("heading", { name: "Sign in" });
    if (navigates) expect(assign).toHaveBeenCalledWith(target);
    else expect(assign).not.toHaveBeenCalled();
  });
