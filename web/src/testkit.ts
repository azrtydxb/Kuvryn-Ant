import { vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";

export type Call = { url: string; init: RequestInit };
export const ids = {
  feed: "00000000-0000-0000-0000-000000000001",
  source: "00000000-0000-0000-0000-000000000002",
  principal: "00000000-0000-0000-0000-000000000003",
  service: "00000000-0000-0000-0000-000000000004",
  role: "00000000-0000-0000-0000-000000000005",
};
export const feed = {
  id: ids.feed,
  slug: "production",
  format: "oci",
  revision: 2,
  anonymous_read: false,
};
export const source = {
  id: ids.source,
  name: "oci-local",
  kind: "local",
  format: "oci",
  revision: 4,
};
export const operator = {
  id: ids.principal,
  subject: "operator",
  kind: "user",
  disabled: false,
  revision: 7,
  role_ids: [] as string[],
};
export const automation = {
  id: ids.service,
  subject: "automation",
  kind: "service",
  disabled: false,
  revision: 1,
  role_ids: [] as string[],
};
export const role = { id: ids.role, name: "readers", revision: 3, grants: [] };
export const page = { has_more: false };
export const json = (v: unknown, status = 200, etag = '"2"') =>
  new Response(JSON.stringify(v), { status, headers: { ETag: etag } });
const notFound = () => json({ code: "not_found" }, 404);

// Routes the management API the way the real service shapes it. Endpoints that
// are not in the contract yet answer 404, as they do on a server without them.
export function mockAPI(
  override: (url: string, init: RequestInit) => Response | undefined = () =>
    undefined,
) {
  const calls: Call[] = [];
  const fetcher = vi.fn(async (url: string, init: RequestInit = {}) => {
    calls.push({ url, init });
    const custom = override(url, init);
    if (custom) return custom;
    if (url === "/auth/session") return new Response("", { status: 401 });
    if (url === "/auth/providers") return json({ providers: [] });
    const path = url.split("?")[0];
    const method = init.method ?? "GET";
    if (path === "/api/v1/status")
      return json({ name: "kuvryn-ant", api_version: "v1" });
    if (path === "/api/v1/feeds" && method === "GET")
      return json({ items: [feed], page });
    if (path === "/api/v1/feeds" && method === "POST") return json(feed);
    // Newer endpoints answer 404 unless a test opts in, like an older server.
    if (path === "/api/v1/feeds/batch") return notFound();
    if (path === `/api/v1/feeds/${ids.feed}` && method !== "DELETE")
      return json(feed);
    if (path === `/api/v1/feeds/${ids.feed}/publish-policy`)
      return json({
        feed_id: ids.feed,
        revision: 2,
        default_source_id: "",
        rules: [],
      });
    if (path === `/api/v1/feeds/${ids.feed}/sources`)
      return json({ items: [], page, feed_revision: 2 });
    if (path.startsWith(`/api/v1/feeds/${ids.feed}/sources/`))
      return json({
        feed_id: ids.feed,
        source_id: ids.source,
        feed_revision: 3,
      });
    if (path === `/api/v1/feeds/${ids.feed}/packages`)
      return json({ items: [], page });
    if (path === "/api/v1/sources" && method === "GET")
      return json({ items: [source], page });
    if (path === "/api/v1/sources" && method === "POST") return json(source);
    if (path === `/api/v1/sources/${ids.source}`) return json(source);
    if (path === "/api/v1/principals" && method === "GET")
      return json({ items: [operator, automation] });
    if (path === "/api/v1/roles" && method === "GET")
      return json({ items: [role] });
    if (path === "/api/v1/webhooks") return json({ items: [] });
    if (path === "/api/v1/me") return json(meToken);
    if (method !== "GET") return json({});
    return notFound();
  });
  return { calls, fetcher };
}

export async function signInWithToken(token = "private-token") {
  fireEvent.click(
    await screen.findByRole("button", { name: "Use an API token" }),
  );
  fireEvent.change(screen.getByLabelText("Short-lived API token"), {
    target: { value: token },
  });
  fireEvent.click(screen.getByRole("button", { name: "Use token" }));
}

export const meToken = {
  principal_id: ids.principal,
  kind: "service",
  display_name: "automation",
  subject: "automation",
  auth_method: "token",
  roles: [] as { id: string; name: string }[],
  password_change: {
    mode: "unavailable",
    reason: "API token sessions have no password.",
  },
  active_sessions: 0,
};

// The account menu lives in the header; its trigger is the avatar button.
export async function openAccountMenu() {
  const trigger = await screen.findByRole("button", { name: /^Account menu/ });
  fireEvent.click(trigger);
  return screen.findByRole("menu");
}
export async function signOutViaMenu() {
  await openAccountMenu();
  fireEvent.click(screen.getByRole("menuitem", { name: "Sign out" }));
}
