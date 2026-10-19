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
import { Identity } from "./Identity";
import { SignIn } from "./SignIn";
import { Client } from "./api";
import { json, mockAPI, type Call } from "./testkit";

const pid = "00000000-0000-0000-0000-0000000000a1";
const provider = {
  id: pid,
  kind: "ldap",
  name: "corp-ad",
  enabled: true,
  config: {
    provider_id: "https://ldap.example",
    url: "ldaps://directory.example",
    bind_dn: "cn=reader,dc=example",
    user_base_dn: "ou=users,dc=example",
    group_base_dn: "ou=groups,dc=example",
    login_attribute: "uid",
    user_id_attribute: "entryUUID",
    group_id_attribute: "entryUUID",
    user_class: "person",
    group_class: "groupOfNames",
    member_attribute: "member",
  },
  has_secret: true,
  revision: 3,
  created_at: "2026-10-01T00:00:00Z",
  updated_at: "2026-10-02T00:00:00Z",
  active: true,
  shadowed_by_file: false,
  certificates: [],
};
const list = {
  items: [provider],
  file_items: [
    { kind: "oidc", label: "https://file-idp.example", detail: "client ant" },
  ],
  active: [{ kind: "ldap", source: "database", provider_id: pid }],
  sso_mandatory: false,
  origin: "https://ant.example",
  local_account: {
    exists: true,
    username: "break-glass",
    totp_enabled: false,
  },
};

let calls: Call[];
function install(extra: Parameters<typeof mockAPI>[0] = () => undefined) {
  const api = mockAPI((url, init) => {
    const custom = extra(url, init);
    if (custom) return custom;
    const path = url.split("?")[0];
    const method = init.method ?? "GET";
    if (path === "/api/v1/identity-providers" && method === "GET")
      return json(list);
    if (path === "/api/v1/identity-providers" && method === "POST")
      return json(provider, 201, '"1"');
    if (path === "/api/v1/identity-providers/test")
      return json({
        kind: "ldap",
        ok: false,
        checks: [
          { name: "tcp", ok: true, detail: "TCP connection established" },
          {
            name: "tls",
            ok: false,
            detail: "TLS negotiation or certificate verification failed",
          },
        ],
        warnings: [],
        tls_mode: "ldaps",
        certificates: [],
      });
    if (path === `/api/v1/identity-providers/${pid}/disable`)
      return json({ ...provider, enabled: false, active: false });
    if (path === `/api/v1/identity-policy`)
      return json({ sso_mandatory: true });
    return undefined;
  });
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
beforeEach(() => install());
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const writes = (method: string) =>
  calls.filter((c) => c.init.method === method);

async function openTab() {
  render(<Identity client={new Client("token")} />);
  await screen.findByText("operator");
  fireEvent.click(screen.getByRole("tab", { name: /Sign-in providers/ }));
  await screen.findByText("corp-ad");
}

it("shows file providers read-only, stored providers with status, and never a secret", async () => {
  await openTab();
  expect(screen.getByText("https://file-idp.example")).toBeTruthy();
  expect(screen.getByText("File")).toBeTruthy();
  expect(screen.getByText("Active")).toBeTruthy();
  expect(screen.getByText(/Present/)).toBeTruthy();
  expect(document.body.textContent).not.toContain("bind-password");
  // File providers offer no actions.
  expect(
    screen.queryByRole("button", { name: /Actions for https:\/\/file/ }),
  ).toBeNull();
});

it("disables a provider with the listed revision as If-Match", async () => {
  await openTab();
  fireEvent.click(screen.getByRole("button", { name: "Actions for corp-ad" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Disable" }));
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  const call = writes("POST")[0];
  expect(call.url).toBe(`/api/v1/identity-providers/${pid}/disable`);
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"3"');
});

it("tests an LDAP configuration and then saves it; the bind password is sent once and cleared", async () => {
  await openTab();
  fireEvent.click(screen.getByRole("button", { name: "Add provider" }));
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "LDAP / Active Directory" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: /Add LDAP \/ Active Directory provider/,
  });
  const set = (label: string | RegExp, value: string) =>
    fireEvent.change(within(dialog).getByLabelText(label), {
      target: { value },
    });
  set("Name", "new-dir");
  set("Provider ID", "https://ldap.example");
  set("Server URL", "ldaps://directory.example");
  set("Bind DN", "cn=reader,dc=example");
  set("Bind password", "bind-password-123");
  set("User base DN", "ou=users,dc=example");
  set("Group base DN", "ou=groups,dc=example");
  set(/Group to role mapping/, "ZGV2=reader,writer");
  set("Group nesting", "direct");
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Test connection" }),
  );
  await within(dialog).findByText("Some checks failed");
  expect(
    within(dialog).getByText(/certificate verification failed/),
  ).toBeTruthy();
  const test = JSON.parse(
    writes("POST").find((c) => c.url.endsWith("/test"))!.init.body as string,
  );
  expect(test.kind).toBe("ldap");
  expect(test.secret).toBe("bind-password-123");
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Save provider" }),
  );
  await waitFor(() =>
    expect(
      writes("POST").some((c) => c.url === "/api/v1/identity-providers"),
    ).toBe(true),
  );
  const create = JSON.parse(
    writes("POST").find((c) => c.url === "/api/v1/identity-providers")!.init
      .body as string,
  );
  expect(create).toMatchObject({
    kind: "ldap",
    name: "new-dir",
    enabled: false,
    secret: "bind-password-123",
    config: {
      user_base_dn: "ou=users,dc=example",
      group_roles: [{ group_id: "ZGV2", role_ids: ["reader", "writer"] }],
      group_nesting: "direct",
    },
  });
  await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
  expect(document.body.textContent).not.toContain("bind-password-123");
});

it("offers discovered OIDC endpoints and previews a pasted token mapping", async () => {
  install((url, init) => {
    if (url.endsWith("/test") && init.method === "POST")
      return json({
        kind: "oidc",
        ok: false,
        checks: [{ name: "approved_endpoints", ok: false, detail: "x" }],
        warnings: ["allowed_endpoints must be set"],
        clock_skew_seconds: 95,
        suggested_allowed_endpoints: [
          "https://idp.example/authorize",
          "https://idp.example/token",
          "https://idp.example/jwks",
        ],
        certificates: [],
      });
    if (url.endsWith(`/${pid}/preview`))
      return json({
        identity: {
          issuer: "https://idp.example",
          subject: "u1",
          groups: ["eng"],
        },
        signature_verified: false,
        binding: { found: false, roles: [] },
        proposed_roles: [],
        would_sign_in: false,
        problems: ["no identity binding for this issuer and subject"],
        warnings: ["signature was not verified"],
        clock_skew_seconds: 600,
      });
    return undefined;
  });
  await openTab();
  fireEvent.click(screen.getByRole("button", { name: "Add provider" }));
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "OpenID Connect" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: /Add OpenID Connect provider/,
  });
  fireEvent.change(within(dialog).getByLabelText("Issuer URL"), {
    target: { value: "https://idp.example" },
  });
  fireEvent.change(within(dialog).getByLabelText("Client ID"), {
    target: { value: "ant" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Test connection" }),
  );
  await within(dialog).findByText(/Clock difference from the provider: 95 s/);
  expect(within(dialog).getByText(/over the 30 s tolerance/)).toBeTruthy();
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Use discovered endpoints" }),
  );
  expect(
    (within(dialog).getByLabelText("Approved endpoints") as HTMLTextAreaElement)
      .value,
  ).toBe(
    "https://idp.example/authorize\nhttps://idp.example/token\nhttps://idp.example/jwks",
  );
  fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

  fireEvent.click(screen.getByRole("button", { name: "Actions for corp-ad" }));
  fireEvent.click(
    await screen.findByRole("menuitem", { name: "Mapping preview" }),
  );
  const preview = await screen.findByRole("dialog", {
    name: /Mapping preview/,
  });
  fireEvent.change(within(preview).getByLabelText("LDAP login"), {
    target: { value: "alice" },
  });
  fireEvent.click(within(preview).getByRole("button", { name: "Preview" }));
  await within(preview).findByText("This identity would be refused");
  expect(
    within(preview).getByText(/no identity binding for this issuer/),
  ).toBeTruthy();
  expect(
    JSON.parse(
      writes("POST").find((c) => c.url.endsWith("/preview"))!.init
        .body as string,
    ),
  ).toEqual({ login: "alice" });
});

it("makes SSO mandatory through the policy endpoint", async () => {
  await openTab();
  fireEvent.click(
    screen.getByLabelText(/Require SSO: disable local password sign-in/),
  );
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  expect(JSON.parse(writes("PUT")[0].init.body as string)).toEqual({
    sso_mandatory: true,
  });
});

it("signs in with the break-glass account, asking for the TOTP code only after the password is right", async () => {
  const session = {
    csrf_token: "A".repeat(43),
    session: {
      PrincipalID: "p",
      InstanceID: "i",
      ExpiresAt: new Date(Date.now() + 3600_000).toISOString(),
    },
  };
  let attempts = 0;
  const bodies: unknown[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit = {}) => {
      if (url === "/auth/providers")
        return json({ providers: [], local: true, sso_mandatory: true });
      if (url === "/auth/session")
        return attempts >= 2
          ? json(session)
          : new Response(null, { status: 401 });
      if (url === "/auth/local/login") {
        attempts++;
        bodies.push(JSON.parse(init.body as string));
        return attempts === 1
          ? json({ mfa_required: true }, 401)
          : new Response("", { status: 200 });
      }
      return json({});
    }),
  );
  const onSession = vi.fn();
  render(<SignIn onSession={onSession} onToken={() => undefined} />);
  await screen.findByLabelText("Username");
  expect(screen.getByText(/Single sign-on is required/)).toBeTruthy();
  fireEvent.change(screen.getByLabelText("Username"), {
    target: { value: "break-glass" },
  });
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: "emergency passphrase 42" },
  });
  expect(screen.queryByLabelText("Authenticator code")).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  const code = await screen.findByLabelText("Authenticator code");
  expect((screen.getByLabelText("Password") as HTMLInputElement).value).toBe(
    "",
  );
  fireEvent.change(screen.getByLabelText("Password"), {
    target: { value: "emergency passphrase 42" },
  });
  fireEvent.change(code, { target: { value: "123456" } });
  fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
  await waitFor(() => expect(onSession).toHaveBeenCalled());
  expect(bodies).toEqual([
    { username: "break-glass", password: "emergency passphrase 42" },
    {
      username: "break-glass",
      password: "emergency passphrase 42",
      totp_code: "123456",
    },
  ]);
  expect(document.body.textContent).not.toContain("emergency passphrase 42");
});

it("offers the named provider and password form first, with the API token behind a link", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string) => {
      if (url === "/auth/providers")
        return json({
          providers: ["oidc"],
          local: true,
          sso_mandatory: false,
          labels: { oidc: "Zitadel" },
        });
      if (url === "/auth/session") return new Response(null, { status: 401 });
      return json({});
    }),
  );
  const onToken = vi.fn();
  render(<SignIn onSession={() => undefined} onToken={onToken} />);
  expect(
    (
      await screen.findByRole("link", { name: "Sign in with Zitadel" })
    ).getAttribute("href"),
  ).toBe("/auth/oidc/login");
  // The password form is visible without any toggle; the token field is not.
  expect(screen.getByLabelText("Username")).toBeTruthy();
  expect(screen.getByLabelText("Password")).toBeTruthy();
  expect(screen.queryByLabelText("Short-lived API token")).toBeNull();
  expect(screen.queryByText(/Emergency access/)).toBeNull();
  fireEvent.click(screen.getByRole("button", { name: "Use an API token" }));
  fireEvent.change(screen.getByLabelText("Short-lived API token"), {
    target: { value: "tok" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Use token" }));
  expect(onToken).toHaveBeenCalledWith("tok");
});
