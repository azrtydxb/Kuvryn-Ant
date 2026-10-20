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
  ids,
  json,
  meToken,
  mockAPI,
  openAccountMenu,
  signInWithToken,
  type Call,
} from "./testkit";

let calls: Call[];
function install(
  me: object,
  extra?: (url: string, init: RequestInit) => Response | undefined,
) {
  const api = mockAPI((url, init) => {
    if (url === "/api/v1/me" && (init.method ?? "GET") === "GET")
      return json(me);
    return extra?.(url, init);
  });
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
const base = {
  ...meToken,
  kind: "user",
  display_name: "Ada Lovelace",
  subject: "ada",
  email: "ada@example.com",
  roles: [{ id: ids.role, name: "readers" }],
  active_sessions: 2,
};
const local = {
  ...base,
  auth_method: "local",
  password_change: { mode: "local" },
};
const zitadel = {
  ...base,
  auth_method: "oidc",
  idp: "Zitadel",
  password_change: {
    mode: "external",
    url: "https://auth.example.com/ui/console/users/me/password",
  },
};
const items = () =>
  screen.getAllByRole("menuitem").map((i) => i.textContent?.trim());
async function menu() {
  render(<App />);
  await signInWithToken();
  await screen.findByRole("button", { name: "Account menu for Ada Lovelace" });
  return openAccountMenu();
}

it("lists header, profile, tokens, change password, theme and sign out for a local user", async () => {
  install(local);
  const m = await menu();
  expect(m.textContent).toContain("Ada Lovelace");
  expect(m.textContent).toContain("ada@example.com");
  expect(m.textContent).toContain("Signed in with Local password");
  expect(m.textContent).toContain("Roles: readers");
  expect(items()).toEqual(
    expect.arrayContaining([
      "My profile",
      "Personal access tokens",
      "Change password",
      "Sign out",
    ]),
  );
  expect(items().some((i) => i?.startsWith("Dark theme"))).toBe(true);
});

it("changes a local password through the modal with policy feedback", async () => {
  install(local, (url, init) =>
    url === "/api/v1/me/password" && init.method === "POST"
      ? new Response(null, { status: 204 })
      : undefined,
  );
  await menu();
  fireEvent.click(screen.getByRole("menuitem", { name: "Change password" }));
  const dialog = await screen.findByRole("dialog", { name: "Change password" });
  const submit = within(dialog).getAllByRole("button", {
    name: "Change password",
  })[0];
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(within(dialog).getByLabelText("Current password"), {
    target: { value: "old password value" },
  });
  fireEvent.change(within(dialog).getByLabelText("New password"), {
    target: { value: "short" },
  });
  fireEvent.change(within(dialog).getByLabelText("Confirm new password"), {
    target: { value: "short" },
  });
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  expect(dialog.textContent).toContain("At least 14 characters");
  fireEvent.change(within(dialog).getByLabelText("New password"), {
    target: { value: "a much longer new password" },
  });
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(within(dialog).getByLabelText("Confirm new password"), {
    target: { value: "a much longer new password" },
  });
  expect((submit as HTMLButtonElement).disabled).toBe(false);
  fireEvent.click(submit);
  await waitFor(() =>
    expect(
      screen.queryByRole("dialog", { name: "Change password" }),
    ).toBeNull(),
  );
  const post = calls.find((c) => c.url === "/api/v1/me/password")!;
  expect(JSON.parse(post.init.body as string)).toEqual({
    current_password: "old password value",
    new_password: "a much longer new password",
  });
  expect(document.body.textContent).not.toContain("a much longer new password");
});

it("explains a wrong current password without leaving the modal", async () => {
  install(local, (url, init) =>
    url === "/api/v1/me/password" && init.method === "POST"
      ? json({ code: "forbidden", message: "echo secret" }, 403)
      : undefined,
  );
  await menu();
  fireEvent.click(screen.getByRole("menuitem", { name: "Change password" }));
  const dialog = await screen.findByRole("dialog", { name: "Change password" });
  fireEvent.change(within(dialog).getByLabelText("Current password"), {
    target: { value: "wrong password value" },
  });
  for (const label of ["New password", "Confirm new password"])
    fireEvent.change(within(dialog).getByLabelText(label), {
      target: { value: "a much longer new password" },
    });
  fireEvent.click(
    within(dialog).getAllByRole("button", { name: "Change password" })[0],
  );
  await screen.findByText("The current password is incorrect.");
  expect(screen.getByRole("dialog", { name: "Change password" })).toBeTruthy();
});

it("sends an identity-provider user to the provider in a new tab", async () => {
  install(zitadel);
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  const m = await menu();
  expect(m.textContent).toContain("Signed in with Zitadel");
  fireEvent.click(
    screen.getByRole("menuitem", { name: /Change password \(opens Zitadel\)/ }),
  );
  expect(open).toHaveBeenCalledWith(
    "https://auth.example.com/ui/console/users/me/password",
    "_blank",
    "noopener,noreferrer",
  );
  expect(screen.queryByRole("dialog")).toBeNull();
});

it("never opens a non-https provider URL", async () => {
  install({
    ...zitadel,
    password_change: { mode: "external", url: "http://evil.example/x" },
  });
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  await menu();
  fireEvent.click(screen.getByRole("menuitem", { name: /Change password/ }));
  expect(open).not.toHaveBeenCalled();
});

it("explains on the profile page when the password is managed elsewhere", async () => {
  install({
    ...base,
    auth_method: "ldap",
    idp: "Corp LDAP",
    password_change: {
      mode: "unavailable",
      reason: "Your password is managed by your directory.",
    },
  });
  const open = vi.spyOn(window, "open").mockReturnValue(null);
  await menu();
  fireEvent.click(screen.getByRole("menuitem", { name: "Change password" }));
  await screen.findByRole("heading", { name: "My profile" });
  expect(
    await screen.findByText("Your password is managed by your directory."),
  ).toBeTruthy();
  expect(open).not.toHaveBeenCalled();
});

it("hides change password for token sessions", async () => {
  install({ ...meToken, display_name: "Ada Lovelace" });
  await menu();
  expect(items()).not.toContain("Change password");
  expect(items()).toContain("Sign out");
});

it("moves with arrow keys and closes with Escape", async () => {
  install(local);
  const trigger = await (async () => {
    await menu();
    return screen.getByRole("button", { name: /^Account menu/ });
  })();
  fireEvent.keyDown(document, { key: "ArrowDown" });
  const first = screen.getAllByRole("menuitem")[0];
  expect(document.activeElement).toBe(first);
  fireEvent.keyDown(document, { key: "ArrowUp" });
  const all = screen.getAllByRole("menuitem");
  expect(document.activeElement).toBe(all[all.length - 1]);
  fireEvent.keyDown(document, { key: "Escape" });
  await waitFor(() => expect(screen.queryByRole("menu")).toBeNull());
  expect(document.activeElement).toBe(trigger);
});

it("shows the identity provider picture when present and initials otherwise", async () => {
  install({ ...zitadel, picture: "https://img.example.com/ada.png" });
  render(<App />);
  await signInWithToken();
  await screen.findByRole("button", { name: /^Account menu/ });
  await waitFor(() =>
    expect(
      document.querySelector(".profile-trigger img")?.getAttribute("src"),
    ).toBe("https://img.example.com/ada.png"),
  );
});

it("shows initials without a picture", async () => {
  install(local);
  await menu();
  expect(document.querySelector(".profile-trigger img")).toBeNull();
  expect(document.querySelector(".profile-trigger")?.textContent).toBe("AL");
});
