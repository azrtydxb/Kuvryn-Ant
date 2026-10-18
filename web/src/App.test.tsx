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
import {
  ids,
  json,
  mockAPI,
  signInWithToken,
  source,
  page,
  type Call,
} from "./testkit";

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
  vi.restoreAllMocks();
});
const nav = (name: string) =>
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name }),
  );
const writes = (method: string) =>
  calls.filter((c) => c.init.method === method);

it("keeps tokens out of browser storage and removes protected data on sign-out", async () => {
  const local = vi.spyOn(Storage.prototype, "setItem");
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  await screen.findByRole("button", { name: /production/ });
  expect(local).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
  expect(screen.queryByRole("button", { name: /production/ })).toBeNull();
  await screen.findByRole("heading", { name: "Sign in" });
  expect(screen.queryByLabelText("Short-lived bearer token")).toBeNull();
  expect(document.body.textContent).not.toContain("private-token");
});
it("loads feed details and saves with the returned ETag", async () => {
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Settings" }));
  fireEvent.change(await screen.findByLabelText("Feed slug"), {
    target: { value: "renamed" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save feed" }));
  await waitFor(() => expect(writes("PATCH")).toHaveLength(1));
  const call = writes("PATCH")[0];
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"2"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    slug: "renamed",
    anonymous_read: false,
  });
});
it("edits a structured deny rule and saves a full policy", async () => {
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("tab", { name: "Publish policy" }));
  fireEvent.click(await screen.findByRole("button", { name: "Add rule" }));
  const dialog = screen.getByRole("dialog", { name: "Add rule" });
  fireEvent.click(within(dialog).getByRole("radio", { name: "Reject write" }));
  fireEvent.change(within(dialog).getByLabelText("Namespace"), {
    target: { value: "private" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Apply" }));
  fireEvent.click(screen.getByRole("button", { name: "Save publish policy" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const body = JSON.parse(writes("PUT")[0].init.body as string);
  expect(body.default_source_id).toBe("");
  expect(body.rules).toHaveLength(1);
  expect(body.rules[0].deny).toBe(true);
  expect(body.rules[0].target_source_id).toBeUndefined();
  expect(body.rules[0].selector.namespace).toBe("private");
});
it("shows permission failures and does not render HTML from remote messages", async () => {
  install((url) =>
    url.startsWith("/api/v1/feeds")
      ? json(
          { code: "forbidden", message: "<img src=x onerror=alert(1)>" },
          403,
        )
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  const alerts = await screen.findAllByRole("alert");
  expect(alerts.some((a) => a.textContent?.includes("Permission denied"))).toBe(
    true,
  );
  expect(document.querySelector("img[src='x']")).toBeNull();
  expect(document.body.textContent).not.toContain("onerror");
});
it("clears an authenticated session when the page is left", async () => {
  render(<App />);
  await signInWithToken();
  await screen.findByRole("button", { name: "Sign out" });
  fireEvent(window, new Event("pagehide"));
  await screen.findByRole("heading", { name: "Sign in" });
  expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
});
it("creates a source using only supported configuration fields", async () => {
  render(<App />);
  await signInWithToken();
  nav("Sources");
  fireEvent.click(await screen.findByRole("button", { name: "Create source" }));
  const dialog = screen.getByRole("dialog", { name: "Create source" });
  fireEvent.change(within(dialog).getByLabelText("Source name"), {
    target: { value: "release-store" },
  });
  fireEvent.change(within(dialog).getByLabelText("Source kind"), {
    target: { value: "local" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Create source" }),
  );
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    name: "release-store",
    kind: "local",
    format: "oci",
  });
});
it("attaches a picked source at an explicit read priority with feed revision protection", async () => {
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("button", { name: "Attach source" }));
  const dialog = await screen.findByRole("dialog", { name: "Attach source" });
  const picker = await within(dialog).findByLabelText("Source");
  // A picker of same-format sources, not a UUID input.
  expect(picker.tagName).toBe("SELECT");
  await within(dialog).findByRole("option", { name: "oci-local (local)" });
  fireEvent.change(within(dialog).getByLabelText("Read priority"), {
    target: { value: "50" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Attach source" }),
  );
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  const call = writes("PUT")[0];
  expect(call.url).toBe(`/api/v1/feeds/${ids.feed}/sources/${ids.source}`);
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"2"');
  expect(JSON.parse(call.init.body as string)).toEqual({ priority: 50 });
});
it("offers only sources of the feed format when attaching", async () => {
  install((url) =>
    url.startsWith("/api/v1/sources?")
      ? json({
          items: [
            source,
            { ...source, id: "x", name: "go-local", format: "go" },
          ],
          page,
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: /production/ }));
  fireEvent.click(await screen.findByRole("button", { name: "Attach source" }));
  const dialog = await screen.findByRole("dialog", { name: "Attach source" });
  await within(dialog).findByRole("option", { name: "oci-local (local)" });
  expect(within(dialog).queryByRole("option", { name: /go-local/ })).toBeNull();
});
it("creates a feed, then attaches the picked sources and sets the publish target", async () => {
  render(<App />);
  await signInWithToken();
  nav("Feeds");
  fireEvent.click(await screen.findByRole("button", { name: "Create feed" }));
  const dialog = screen.getByRole("dialog", { name: "Create feed" });
  fireEvent.change(within(dialog).getByLabelText("Feed slug"), {
    target: { value: "release" },
  });
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  fireEvent.click(await within(dialog).findByLabelText(/oci-local/));
  fireEvent.click(within(dialog).getByRole("button", { name: "Continue" }));
  fireEvent.click(within(dialog).getByRole("button", { name: "Create feed" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(2));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    slug: "release",
    format: "oci",
    anonymous_read: false,
  });
  expect(JSON.parse(writes("PUT")[0].init.body as string)).toEqual({
    priority: 10,
  });
  expect(JSON.parse(writes("PUT")[1].init.body as string)).toEqual({
    default_source_id: ids.source,
    rules: [],
  });
});
it("shows a clear unavailable state for features the server does not provide yet", async () => {
  render(<App />);
  await signInWithToken();
  nav("Audit log");
  await screen.findByText("Not available on this server yet");
  nav("Packages");
  await screen.findByText(
    /Cross-feed search: Not available on this server yet/,
  );
  // Per-feed browsing keeps working.
  await screen.findByLabelText("Package prefix");
});
it("uses cross-feed package search when the server provides it", async () => {
  install((url) =>
    url.startsWith("/api/v1/packages")
      ? json({
          items: [
            {
              id: "p1",
              coordinate: "team/app",
              namespace: "team",
              feed_id: ids.feed,
              feed_slug: "production",
              format: "oci",
              source_id: ids.source,
              latest_version: "v2.14.0",
            },
          ],
          page,
          total: 41,
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Packages");
  await screen.findByText("team/app");
  expect(screen.getByText(/of 41 packages/)).toBeTruthy();
});
it("toggles light and dark themes", async () => {
  render(<App />);
  await signInWithToken();
  fireEvent.click(
    await screen.findByRole("button", { name: /Switch to (light|dark) theme/ }),
  );
  expect(["light", "dark"]).toContain(document.documentElement.dataset.theme);
});
