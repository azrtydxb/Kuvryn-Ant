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
import { Webhooks } from "./Webhooks";
import { ToastProvider } from "./ui";
import { Client } from "./api";
import { feed, json, mockAPI, page, source, type Call } from "./testkit";

const config = {
  public_url: "https://ant.example.com",
  hook_base_url: "https://ant.example.com/hook/",
  max_body_bytes: 1048576,
  log_retention_days: 14,
  hmac_formats: [
    { format: "github", header: "X-Hub-Signature-256" },
    { format: "gitlab", header: "X-Gitlab-Token" },
    { format: "ant", header: "X-Ant-Signature" },
    { format: "npm", header: "X-NPM-Signature" },
    { format: "artifactory", header: "X-JFrog-Event-Auth" },
    { format: "nexus", header: "X-Nexus-Webhook-Signature" },
  ],
};
const secret = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "production",
  description: "",
  created_at: "2026-01-01T00:00:00Z",
  revision: 1,
  used_by_count: 0,
  used_by: [],
};
const hookBase = {
  description: "GitHub releases",
  enabled: true,
  rate_per_minute: 60,
  revision: 3,
  created_at: "2026-01-02T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
  received_7d: 10,
  succeeded_7d: 9,
  failed_7d: 1,
};
const release = {
  ...hookBase,
  id: "aaaaaaaa-0000-0000-0000-000000000001",
  slug: "release-notify",
  auth_mode: "hmac",
  hmac_format: "github",
  signing_secret_id: secret.id,
  signing_secret_name: "production",
  action: { type: "upstream_notify", provider: "github", prewarm: false },
  action_summary: "Upstream notification (github)",
  path: "/hook/release-notify",
  url: "https://ant.example.com/hook/release-notify",
  last_received_at: "2026-01-05T10:00:00Z",
  success_rate: 0.9,
};
const dockerhub = {
  ...hookBase,
  id: "aaaaaaaa-0000-0000-0000-000000000002",
  slug: "hub-push",
  description: "",
  auth_mode: "url_token",
  token_hint: "a1b2",
  action: { type: "upstream_notify", provider: "dockerhub", prewarm: true },
  action_summary: "Upstream notification (dockerhub) + prewarm",
  path: "/hook/hub-push",
  url: "https://ant.example.com/hook/hub-push",
  received_7d: 0,
  succeeded_7d: 0,
  failed_7d: 0,
};
const delivery = (over: Record<string, unknown>) => ({
  id: "dddddddd-0000-0000-0000-000000000001",
  received_at: "2026-01-05T10:00:00Z",
  source_ip: "203.0.113.9",
  auth_result: "ok",
  state: "succeeded",
  attempts: 1,
  detail: { provider: "github", sources_matched: 1 },
  payload_size: 2048,
  preview: '{"release":{"tag_name":"v1"},"token":"[redacted]"}',
  action_result: "cache_invalidated",
  ...over,
});
const deliveries = [
  delivery({}),
  delivery({
    id: "dddddddd-0000-0000-0000-000000000002",
    auth_result: "invalid",
    state: "rejected",
    action_result: "invalid",
    preview: "",
    payload_size: 12,
  }),
];

let calls: Call[];
let hooksList: unknown[];
let unavailable = false;
const cacheFeed = {
  ...feed,
  id: "00000000-0000-0000-0000-0000000000f2",
  slug: "buildcache",
  purpose: "cache",
};

function install() {
  const api = mockAPI((url, init) => {
    const path = url.split("?")[0];
    const method = init.method ?? "GET";
    if (unavailable && path.startsWith("/api/v1/incoming-hook"))
      return json({ code: "not_found" }, 404);
    if (path === "/api/v1/incoming-hook-config") return json(config);
    if (path === "/api/v1/incoming-hooks" && method === "GET")
      return json({ items: hooksList });
    if (path === "/api/v1/incoming-hooks" && method === "POST") {
      const body = JSON.parse(init.body as string);
      return json(
        {
          ...hookBase,
          ...body,
          id: "aaaaaaaa-0000-0000-0000-000000000009",
          action_summary: "Created",
          path: `/hook/${body.slug}`,
          url: `https://ant.example.com/hook/${body.slug}`,
          revision: 1,
          received_7d: 0,
          succeeded_7d: 0,
          failed_7d: 0,
          ...(body.auth_mode === "url_token"
            ? { token: "hk_once-only-secret-value", token_hint: "alue" }
            : {}),
        },
        201,
        '"1"',
      );
    }
    if (path.endsWith("/deliveries") && method === "GET")
      return json({ items: deliveries });
    if (path.endsWith("/test"))
      return json({ delivery_id: "dddddddd-0000-0000-0000-0000000000aa" }, 202);
    if (path.endsWith("/redeliver"))
      return json({ delivery_id: "dddddddd-0000-0000-0000-0000000000bb" }, 202);
    if (path.endsWith("/token") && method === "POST")
      return json(
        { ...dockerhub, token: "hk_regenerated-secret", revision: 4 },
        200,
        '"4"',
      );
    if (path === "/api/v1/webhook-secrets") return json({ items: [secret] });
    if (path === "/api/v1/feeds" && method === "GET")
      return json({ items: [feed, cacheFeed], page });
    if (path === "/api/v1/sources" && method === "GET")
      return json({
        items: [
          source,
          {
            ...source,
            id: "00000000-0000-0000-0000-0000000000a1",
            name: "docker-hub",
            kind: "remote",
            origin: "https://registry-1.docker.io",
          },
        ],
        page,
      });
    if (path === "/api/v1/replication/jobs") return json({ items: [] });
    return undefined;
  });
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
beforeEach(() => {
  hooksList = [release, dockerhub];
  unavailable = false;
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const renderPage = (tab?: "incoming" | "outgoing") =>
  render(
    <ToastProvider>
      <Webhooks client={new Client("token")} initialTab={tab} />
    </ToastProvider>,
  );
const writes = (method: string) =>
  calls.filter((c) => c.init.method === method);

it("has Incoming and Outgoing tabs with a list and a create action each", async () => {
  renderPage();
  const tabs = screen.getAllByRole("tab");
  expect(tabs.map((t) => t.textContent)).toEqual(["Incoming", "Outgoing"]);
  expect(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  ).toBeTruthy();
  fireEvent.click(screen.getByRole("tab", { name: "Outgoing" }));
  expect(
    await screen.findByRole("button", { name: "Create webhook" }),
  ).toBeTruthy();
  expect(
    screen.queryByRole("button", { name: "Create incoming hook" }),
  ).toBeNull();
  fireEvent.click(screen.getByRole("tab", { name: "Incoming" }));
  expect(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  ).toBeTruthy();
});

it("lists slug, copyable URL, auth mode, action, last received and success rate", async () => {
  renderPage();
  const row = (
    await screen.findByRole("button", { name: /^release-notify/ })
  ).closest("tr")!;
  expect(within(row).getByText("GitHub releases")).toBeTruthy();
  expect(
    within(row).getByRole("button", { name: "Copy URL of release-notify" }),
  ).toBeTruthy();
  expect(row.textContent).toContain(
    "https://ant.example.com/hook/release-notify",
  );
  expect(row.textContent).toContain("HMAC · GitHub");
  expect(row.textContent).toContain("Upstream notification (github)");
  expect(row.textContent).toContain("90% of 10 succeeded");
  const token = screen
    .getByRole("button", { name: /^hub-push/ })
    .closest("tr")!;
  expect(token.textContent).toContain("Secret in URL");
  expect(token.textContent).toContain("…a1b2");
  expect(token.textContent).toContain("Never");
});

it("builds the live URL from the public URL as the slug is typed", async () => {
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: "Create incoming hook",
  });
  const preview = () => within(dialog).getByTestId("url-preview").textContent;
  expect(preview()).toContain("https://ant.example.com/hook/<slug>");
  const slug = within(dialog).getByLabelText("Slug");
  fireEvent.change(slug, { target: { value: "build-events" } });
  expect(preview()).toContain("https://ant.example.com/hook/build-events");
  expect(
    within(dialog).getByRole("button", { name: "Copy hook URL" }),
  ).toBeTruthy();
  fireEvent.change(slug, { target: { value: "Bad_Slug" } });
  expect(within(dialog).getByText(/Lowercase letters/)).toBeTruthy();
  expect(
    within(dialog).queryByRole("button", { name: "Copy hook URL" }),
  ).toBeNull();
  expect(
    within(dialog).getByRole("button", { name: "Create hook" }),
  ).toHaveProperty("disabled", true);
  // The secret-in-URL mode announces the extra path segment.
  fireEvent.change(within(dialog).getByLabelText("Authentication"), {
    target: { value: "url_token" },
  });
  fireEvent.change(slug, { target: { value: "hub" } });
  expect(preview()).toContain("/<secret token, shown once>");
});

it("creates an HMAC hook with the chosen format, secret and action", async () => {
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: "Create incoming hook",
  });
  const submit = within(dialog).getByRole("button", { name: "Create hook" });
  fireEvent.change(within(dialog).getByLabelText("Slug"), {
    target: { value: "gh-releases" },
  });
  expect(submit).toHaveProperty("disabled", true); // no signing secret yet
  fireEvent.change(within(dialog).getByLabelText("Signature format"), {
    target: { value: "gitlab" },
  });
  fireEvent.change(within(dialog).getByLabelText("Signing secret"), {
    target: { value: secret.id },
  });
  expect(submit).toHaveProperty("disabled", false);
  fireEvent.change(within(dialog).getByLabelText("Provider"), {
    target: { value: "gitlab" },
  });
  fireEvent.click(submit);
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    slug: "gh-releases",
    description: "",
    enabled: true,
    auth_mode: "hmac",
    hmac_format: "gitlab",
    signing_secret_id: secret.id,
    rate_per_minute: 60,
    action: { type: "upstream_notify", provider: "gitlab", prewarm: false },
  });
});

it("offers the fields of each action and validates them", async () => {
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: "Create incoming hook",
  });
  fireEvent.change(within(dialog).getByLabelText("Authentication"), {
    target: { value: "bearer" },
  });
  expect(within(dialog).getByLabelText("Token scope")).toBeTruthy();
  fireEvent.change(within(dialog).getByLabelText("Slug"), {
    target: { value: "cache-clean" },
  });
  const action = within(dialog).getByLabelText("Action");
  fireEvent.change(action, { target: { value: "clean_cache" } });
  // Only cache feeds are offered for cleaning.
  const feeds = within(dialog).getByLabelText(
    "Cache feed",
  ) as HTMLSelectElement;
  expect([...feeds.options].map((o) => o.textContent)).toEqual([
    "Select…",
    "buildcache (oci)",
  ]);
  const submit = within(dialog).getByRole("button", { name: "Create hook" });
  expect(submit).toHaveProperty("disabled", true);
  fireEvent.change(feeds, { target: { value: cacheFeed.id } });
  fireEvent.change(within(dialog).getByLabelText("Prefix template"), {
    target: { value: "{{ .repository }}:pr-{{ .number }}" },
  });
  expect(submit).toHaveProperty("disabled", false);
  fireEvent.click(submit);
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toMatchObject({
    auth_mode: "bearer",
    bearer_scope: "configure",
    action: {
      type: "clean_cache",
      feed_id: cacheFeed.id,
      prefix: "{{ .repository }}:pr-{{ .number }}",
    },
  });
  // Relay: https only.
  cleanup();
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  );
  const d2 = await screen.findByRole("dialog", {
    name: "Create incoming hook",
  });
  fireEvent.change(within(d2).getByLabelText("Slug"), {
    target: { value: "fwd" },
  });
  fireEvent.change(within(d2).getByLabelText("Authentication"), {
    target: { value: "url_token" },
  });
  fireEvent.change(within(d2).getByLabelText("Action"), {
    target: { value: "relay" },
  });
  const submit2 = within(d2).getByRole("button", { name: "Create hook" });
  fireEvent.change(within(d2).getByLabelText("Target URL"), {
    target: { value: "http://internal.example.com/x" },
  });
  expect(submit2).toHaveProperty("disabled", true);
  fireEvent.change(within(d2).getByLabelText("Target URL"), {
    target: { value: "https://internal.example.com/x" },
  });
  expect(submit2).toHaveProperty("disabled", false);
});

it("shows the secret URL of a new token hook once", async () => {
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create incoming hook" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: "Create incoming hook",
  });
  fireEvent.change(within(dialog).getByLabelText("Slug"), {
    target: { value: "hub-new" },
  });
  fireEvent.change(within(dialog).getByLabelText("Authentication"), {
    target: { value: "url_token" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Create hook" }));
  const shown = await screen.findByTestId("hook-url");
  expect(shown.textContent).toContain(
    "https://ant.example.com/hook/hub-new/hk_once-only-secret-value",
  );
  expect(screen.getByText("Store it now")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "I have stored it" }));
  await waitFor(() => expect(screen.queryByTestId("hook-url")).toBeNull());
  expect(document.body.textContent).not.toContain("hk_once-only-secret-value");
});

it("shows the delivery log, previews, test sends, redelivery and token regeneration", async () => {
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: /^hub-push/ }));
  const drawer = await screen.findByRole("dialog");
  const rows = await within(drawer).findAllByRole("row");
  expect(rows.length).toBeGreaterThan(2);
  const log = within(drawer).getByText("Delivery log").parentElement!;
  expect(log.textContent).toContain("203.0.113.9");
  expect(log.textContent).toContain("cache_invalidated");
  expect(log.textContent).toContain("rejected");

  // Details: redacted preview.
  fireEvent.click(within(drawer).getByText("cache_invalidated").closest("tr")!);
  const detail = await screen.findByRole("dialog", { name: "Delivery" });
  expect(
    within(detail).getByLabelText("Payload preview").textContent,
  ).toContain("[redacted]");
  fireEvent.click(within(detail).getByRole("button", { name: "Done" }));

  // Redeliver (only deliveries that have a payload).
  const buttons = within(drawer).getAllByRole("button", { name: "Redeliver" });
  expect(buttons).toHaveLength(1);
  fireEvent.click(buttons[0]);
  await waitFor(() =>
    expect(
      writes("POST").some((c) =>
        c.url.endsWith(
          `/deliveries/dddddddd-0000-0000-0000-000000000001/redeliver`,
        ),
      ),
    ).toBe(true),
  );

  fireEvent.click(within(drawer).getByRole("button", { name: "Send test" }));
  await waitFor(() =>
    expect(
      writes("POST").some((c) =>
        c.url.endsWith(`/api/v1/incoming-hooks/${dockerhub.id}/test`),
      ),
    ).toBe(true),
  );

  fireEvent.click(
    within(drawer).getByRole("button", { name: "Regenerate token" }),
  );
  const shown = await screen.findByTestId("hook-url");
  expect(shown.textContent).toContain("/hub-push/hk_regenerated-secret");
});

it("saves edits with the revision as If-Match and deletes after confirmation", async () => {
  renderPage();
  fireEvent.click(
    await screen.findByRole("button", { name: /^release-notify/ }),
  );
  const drawer = await screen.findByRole("dialog");
  const description = within(drawer).getByLabelText("Description");
  const save = within(drawer).getByRole("button", { name: "Save hook" });
  expect(save).toHaveProperty("disabled", true);
  fireEvent.change(description, { target: { value: "Releases of the tool" } });
  expect(save).toHaveProperty("disabled", false);
  fireEvent.click(save);
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  const patch = writes("PATCH")[0];
  expect(patch.url).toBe(`/api/v1/incoming-hooks/${release.id}`);
  expect(new Headers(patch.init.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(patch.init.body as string).description).toBe(
    "Releases of the tool",
  );
  fireEvent.click(within(drawer).getByRole("button", { name: "Delete" }));
  const confirm = await screen.findByRole("dialog", {
    name: "Delete incoming hook",
  });
  fireEvent.click(within(confirm).getByRole("button", { name: "Delete hook" }));
  await waitFor(() => expect(writes("DELETE")).toHaveLength(1));
});

it("explains an empty list and a server without incoming hooks", async () => {
  hooksList = [];
  renderPage();
  expect(await screen.findByText("No incoming hooks")).toBeTruthy();
  cleanup();
  unavailable = true;
  renderPage();
  expect(await screen.findByText(/not available/i)).toBeTruthy();
});
