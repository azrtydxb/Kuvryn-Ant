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
  mockAPI,
  signInWithToken,
  feed,
  type Call,
} from "./testkit";

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
const nav = (name: string) =>
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name }),
  );
const writes = (m: string) => calls.filter((c) => c.init.method === m);

const limits = {
  revision: 3,
  rate_limits: [
    {
      scope: "feed",
      subject: ids.feed,
      requests_per_minute: 600,
      burst: 100,
    },
  ],
  quotas: [
    {
      scope: "feed",
      feed_id: ids.feed,
      max_bytes: 2 * 1024 ** 3,
      max_packages: 50,
    },
  ],
};
const usage = {
  quotas: [
    {
      scope: "feed",
      feed_id: ids.feed,
      bytes_used: 1.9 * 1024 ** 3,
      packages_used: 7,
      max_bytes: 2 * 1024 ** 3,
      max_packages: 50,
    },
  ],
  throttled: { principal: 0, feed: 4, namespace: 0, site: 1 },
  measured_at: "2026-01-01T00:00:00Z",
};

it("shows limits with quota usage and saves the whole document with its revision", async () => {
  install((url, init) => {
    if (url === "/api/v1/limits" && init.method === "PUT")
      return json({ ...limits, revision: 4 });
    if (url === "/api/v1/limits") return json(limits);
    if (url === "/api/v1/limits/usage") return json(usage);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(
    await screen.findByRole("button", { name: /Limits & quotas/ }),
  );
  await screen.findByText("Request rate limits");
  await screen.findByText(/feed 4 · namespace 0 · site 1/);
  expect(await screen.findByText("near limit")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "Add limit" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Add request limit",
  });
  fireEvent.change(within(dialog).getByLabelText("Requests per minute"), {
    target: { value: "30" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save limit" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const call = writes("PUT")[0];
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"3"');
  const body = JSON.parse(call.init.body as string);
  expect(body.quotas).toEqual(limits.quotas);
  expect(body.rate_limits).toHaveLength(2);
  expect(body.rate_limits[1]).toEqual({
    scope: "feed",
    subject: feed.id,
    requests_per_minute: 30,
    burst: 100,
  });
});

it("reports a rejected limits change with the server's reason", async () => {
  install((url, init) => {
    if (url === "/api/v1/limits" && init.method === "PUT")
      return json(
        {
          code: "invalid_request",
          message:
            "invalid limits: quotas[0]: max_bytes or max_packages required",
        },
        400,
      );
    if (url === "/api/v1/limits") return json(limits);
    if (url === "/api/v1/limits/usage") return json(usage);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(
    await screen.findByRole("button", { name: /Limits & quotas/ }),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Add quota" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Add storage quota",
  });
  fireEvent.change(within(dialog).getByLabelText("Max packages"), {
    target: { value: "5" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Save quota" }));
  await within(dialog).findByText(/max_bytes or max_packages required/);
});

it("creates a template and applies it to a feed with its effective view", async () => {
  const tmpl = {
    id: "00000000-0000-0000-0000-0000000000a1",
    name: "release",
    description: "",
    kind: "publish",
    revision: 1,
    bound_feeds: 0,
    publish: { default_source_id: ids.source, rules: [] },
  };
  install((url, init) => {
    if (url === "/api/v1/policy-templates" && init.method === "POST")
      return json(tmpl, 201, '"1"');
    if (url === "/api/v1/policy-templates") return json({ items: [tmpl] });
    if (
      url === `/api/v1/feeds/${ids.feed}/policy-templates` &&
      init.method === "PUT"
    )
      return json({ feed_id: ids.feed, revision: 3 });
    if (url === `/api/v1/feeds/${ids.feed}/policy-templates`)
      return json({ feed_id: ids.feed, revision: 2 });
    if (url === `/api/v1/feeds/${ids.feed}/effective-policy`)
      return json({
        feed_id: ids.feed,
        revision: 2,
        publish_template_chain: ["release"],
        read_template_chain: [],
        default_source_id: ids.source,
        default_origin: "template:release",
        rules: [
          {
            id: "deny-secret",
            priority: 10,
            selector: {},
            deny: true,
            origin: "template:release",
          },
        ],
        read: {},
        read_origins: {},
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Settings");
  fireEvent.click(
    await screen.findByRole("button", { name: /Policy templates/ }),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Add template" }));
  const dialog = await screen.findByRole("dialog", { name: "Add template" });
  fireEvent.change(within(dialog).getByLabelText("Name"), {
    target: { value: "release" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Save template" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    name: "release",
    description: "",
    kind: "publish",
    publish: { default_source_id: "", rules: [] },
  });
  cleanup();
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Publish policy" }));
  await screen.findByText("Effective configuration");
  expect(await screen.findByText("deny-secret")).toBeTruthy();
  fireEvent.change(await screen.findByLabelText("Publish template"), {
    target: { value: tmpl.id },
  });
  fireEvent.click(screen.getByRole("button", { name: "Apply templates" }));
  await waitFor(() =>
    expect(
      calls.filter(
        (c) =>
          c.init.method === "PUT" &&
          c.url === `/api/v1/feeds/${ids.feed}/policy-templates`,
      ),
    ).toHaveLength(1),
  );
});
