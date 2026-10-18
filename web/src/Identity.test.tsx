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
import { Client } from "./api";
import { ids, json, mockAPI, type Call } from "./testkit";

let calls: Call[];
function install(...args: Parameters<typeof mockAPI>) {
  const api = mockAPI(...args);
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
const tab = (name: string) =>
  fireEvent.click(screen.getByRole("tab", { name: new RegExp(name) }));

it("binds exact issuer and immutable subject to an explicit existing user", async () => {
  render(<Identity client={new Client("token")} />);
  await screen.findByText("operator");
  tab("Sign-in providers");
  fireEvent.click(screen.getByRole("button", { name: "Bind identity" }));
  const dialog = await screen.findByRole("dialog", { name: "Bind identity" });
  // The local user is picked by name, not typed as a UUID.
  expect(within(dialog).getByLabelText("Local user").tagName).toBe("SELECT");
  fireEvent.change(
    within(dialog).getByLabelText("Provider issuer / identifier"),
    {
      target: { value: "https://issuer.example" },
    },
  );
  fireEvent.change(
    within(dialog).getByLabelText("Immutable provider subject"),
    {
      target: { value: "immutable-subject" },
    },
  );
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Create identity binding" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    issuer: "https://issuer.example",
    subject: "immutable-subject",
    principal_id: ids.principal,
  });
});
it("replaces memberships from a role picker using the listed principal revision", async () => {
  render(<Identity client={new Client("token")} />);
  await screen.findByText("operator");
  fireEvent.click(screen.getByRole("button", { name: "Actions for operator" }));
  fireEvent.click(await screen.findByRole("menuitem", { name: "Edit roles" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Roles for operator",
  });
  fireEvent.click(within(dialog).getByLabelText(/readers/));
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Replace role memberships" }),
  );
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const call = writes("PUT")[0];
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"7"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    role_ids: [ids.role],
  });
});
it("edits structured grants with the role revision and no unsupported scope fields", async () => {
  render(<Identity client={new Client("token")} />);
  tab("Roles");
  fireEvent.click(await screen.findByText("readers"));
  const drawer = await screen.findByRole("dialog", { name: "readers" });
  fireEvent.click(within(drawer).getByRole("button", { name: "Add grant" }));
  fireEvent.change(within(drawer).getByLabelText("Resource kind"), {
    target: { value: "instance" },
  });
  fireEvent.click(within(drawer).getByLabelText("admin"));
  fireEvent.click(
    within(drawer).getByRole("button", { name: "Replace role grants" }),
  );
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const call = writes("PUT")[0];
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    grants: [{ kind: "instance", effect: "allow", actions: ["read", "admin"] }],
  });
});
it("picks feeds for grants instead of typing UUIDs", async () => {
  render(<Identity client={new Client("token")} />);
  tab("Roles");
  fireEvent.click(await screen.findByText("readers"));
  const drawer = await screen.findByRole("dialog", { name: "readers" });
  fireEvent.click(within(drawer).getByRole("button", { name: "Add grant" }));
  const feed = within(drawer).getByLabelText("Feed");
  expect(feed.tagName).toBe("SELECT");
  await within(drawer).findByRole("option", { name: "production" });
});
it("issues a token once without a replay key and clears the secret on request", async () => {
  install((url, init) =>
    init.method === "POST" && url.endsWith("/tokens")
      ? json({
          id: "tok-1",
          secret: "s3cret-value",
          expires_at: "2030-01-01T00:00:00Z",
        })
      : undefined,
  );
  render(<Identity client={new Client("token")} />);
  await screen.findByText("operator");
  tab("Tokens");
  fireEvent.click(screen.getByRole("button", { name: "Issue token" }));
  const dialog = await screen.findByRole("dialog", { name: "Issue token" });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Issue token once" }),
  );
  const secret = (await within(dialog).findByLabelText(
    /New token secret/,
  )) as HTMLInputElement;
  expect(secret.type).toBe("password");
  expect(secret.value).toBe("s3cret-value");
  expect(document.body.textContent).not.toContain("s3cret-value");
  const call = writes("POST")[0];
  expect(call.url).toBe(`/api/v1/principals/${ids.service}/tokens`);
  expect(new Headers(call.init.headers).has("Idempotency-Key")).toBe(false);
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Clear displayed secret" }),
  );
  expect(within(dialog).queryByLabelText(/New token secret/)).toBeNull();
});
it("retains issuance failure feedback and requires a distinct new attempt", async () => {
  install((url, init) =>
    init.method === "POST" && url.endsWith("/tokens")
      ? new Response("{}", { status: 503 })
      : undefined,
  );
  render(<Identity client={new Client("token")} />);
  await screen.findByText("operator");
  tab("Tokens");
  fireEvent.click(screen.getByRole("button", { name: "Issue token" }));
  const dialog = await screen.findByRole("dialog", { name: "Issue token" });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Issue token once" }),
  );
  await within(dialog).findByText(/Service unavailable/);
  expect(
    within(dialog).queryByRole("button", { name: "Issue token once" }),
  ).toBeNull();
  expect(
    within(dialog).getByRole("button", {
      name: "Start a separate token issuance",
    }),
  ).toBeTruthy();
});
it("falls back to revoking by token ID when the token list is not available", async () => {
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText(/Token list: Not available on this server yet/);
  fireEvent.change(screen.getByLabelText("Token ID to revoke"), {
    target: { value: ids.service },
  });
  fireEvent.click(screen.getByLabelText("Revoke this token immediately"));
  fireEvent.click(screen.getByRole("button", { name: "Revoke token" }));
  await waitFor(() => expect(writes("DELETE")).toHaveLength(1));
  expect(writes("DELETE")[0].url).toBe(`/api/v1/tokens/${ids.service}`);
});
it("lists tokens with row-level revoke and expiry warnings when the server provides them", async () => {
  const soon = new Date(Date.now() + 5 * 86400000).toISOString();
  install((url) =>
    url.startsWith("/api/v1/tokens?")
      ? json({
          items: [
            { id: ids.service, principal_id: ids.service, expires_at: soon },
          ],
        })
      : undefined,
  );
  render(<Identity client={new Client("token")} />);
  tab("Tokens");
  await screen.findByText(/expire within 14 days/);
  fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
  const dialog = await screen.findByRole("dialog", { name: "Revoke token" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Revoke token" }));
  await waitFor(() => expect(writes("DELETE")).toHaveLength(1));
});
