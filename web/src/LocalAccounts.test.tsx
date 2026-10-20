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
import { Identity } from "./Identity";
import { Client } from "./api";
import { ids, json, mockAPI, type Call } from "./testkit";

let calls: Call[];
let locked = true;
let totp = true;
function install(opts: { self?: boolean; selfEnrol?: boolean } = {}) {
  const api = mockAPI((url, init) => {
    const path = url.split("?")[0];
    if (path === "/api/v1/local-accounts" && (init.method ?? "GET") === "GET")
      return json({
        items: [
          {
            username: "operator",
            principal_id: ids.principal,
            break_glass: false,
            totp_enabled: totp,
            ...(locked
              ? { locked_until: new Date(Date.now() + 3600e3).toISOString() }
              : {}),
          },
        ],
      });
    if (path === "/api/v1/local-account/me")
      return opts.self
        ? json({
            username: "operator",
            principal_id: ids.principal,
            break_glass: false,
            totp_enabled: totp,
          })
        : json({ code: "not_found" }, 404);
    if (path === "/api/v1/identity-policy" && (init.method ?? "GET") === "GET")
      return json({
        sso_mandatory: false,
        local_user_totp: !!opts.selfEnrol,
      });
    if (path === "/api/v1/local-accounts/operator/unlock") {
      locked = false;
      return new Response(null, { status: 204 });
    }
    if (path === "/api/v1/local-accounts/operator/totp") {
      totp = false;
      return new Response(null, { status: 204 });
    }
    if (path === "/api/v1/local-account/totp" && init.method === "POST")
      return json({ otpauth_uri: "otpauth://totp/Ant:operator?secret=ABC" });
    if (path === "/api/v1/local-account/totp/confirm")
      return new Response(null, { status: 204 });
    return undefined;
  });
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  locked = true;
  totp = true;
});
const menu = async (name: string) => {
  fireEvent.click(
    await screen.findByRole("button", { name: "Actions for operator" }),
  );
  fireEvent.click(await screen.findByRole("menuitem", { name }));
};

it("shows lockout and second-factor state for local users and unlocks without a password reset", async () => {
  install();
  render(<Identity client={new Client("token")} />);
  await screen.findByText("Locked");
  expect(screen.getByText("2FA on")).toBeTruthy();
  await menu("Unlock account");
  await waitFor(() =>
    expect(
      calls.some(
        (c) =>
          c.init.method === "POST" &&
          c.url === "/api/v1/local-accounts/operator/unlock",
      ),
    ).toBe(true),
  );
  await waitFor(() => expect(screen.queryByText("Locked")).toBeNull());
});

it("resets a lost second factor", async () => {
  install();
  render(<Identity client={new Client("token")} />);
  await screen.findByText("2FA on");
  await menu("Reset two-factor");
  await waitFor(() =>
    expect(
      calls.some(
        (c) =>
          c.init.method === "DELETE" &&
          c.url === "/api/v1/local-accounts/operator/totp",
      ),
    ).toBe(true),
  );
  await screen.findByText("2FA off");
});

it("lets a signed-in local user enrol their own second factor when the setting allows it", async () => {
  totp = false;
  install({ self: true, selfEnrol: true });
  render(<Identity client={new Client("token")} />);
  await screen.findByText("2FA off");
  await menu("Set up two-factor");
  const dialog = await screen.findByRole("dialog", {
    name: /Two-factor sign-in for operator/,
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Generate secret" }),
  );
  await within(dialog).findByLabelText(/otpauth URI/);
  fireEvent.change(within(dialog).getByLabelText("Code"), {
    target: { value: "123456" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Enable two-factor" }),
  );
  await waitFor(() =>
    expect(
      calls.some(
        (c) =>
          c.init.method === "POST" &&
          c.url === "/api/v1/local-account/totp/confirm",
      ),
    ).toBe(true),
  );
  const confirm = calls.find((c) => c.url.endsWith("/totp/confirm"))!;
  expect(JSON.parse(confirm.init.body as string)).toEqual({ code: "123456" });
});

it("does not offer enrolment unless the setting is on", async () => {
  totp = false;
  install({ self: true, selfEnrol: false });
  render(<Identity client={new Client("token")} />);
  await screen.findByText("2FA off");
  fireEvent.click(
    await screen.findByRole("button", { name: "Actions for operator" }),
  );
  await screen.findByRole("menuitem", { name: "Edit roles" });
  expect(
    screen.queryByRole("menuitem", { name: "Set up two-factor" }),
  ).toBeNull();
});
