// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import App from "./App";
const feed = {
  id: "00000000-0000-0000-0000-000000000001",
  slug: "production",
  format: "oci",
  revision: 2,
  anonymous_read: false,
};
let calls: { url: string; init: RequestInit }[];
beforeEach(() => {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      if (url === "/auth/session") return new Response("", { status: 401 });
      if (url === "/auth/providers")
        return new Response(JSON.stringify({ providers: [] }));
      const data = url.endsWith("/status")
        ? { name: "kuvryn-ant", api_version: "v1" }
        : url.endsWith("/publish-policy")
          ? { feed_id: feed.id, revision: 2, default_source_id: "", rules: [] }
          : url.includes("/packages")
            ? { items: [], page: { has_more: false } }
            : url.includes("/sources?")
              ? { items: [], page: { has_more: false }, feed_revision: 2 }
              : url.includes("/feeds?")
                ? { items: [feed], page: { has_more: false } }
                : feed;
      return new Response(JSON.stringify(data), { headers: { ETag: '"2"' } });
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});
async function login() {
  fireEvent.click(
    await screen.findByText(
      "Service token fallback · development / administration",
    ),
  );
  fireEvent.change(screen.getByLabelText("Short-lived bearer token"), {
    target: { value: "private-token" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Use token" }));
}
it("keeps tokens out of browser storage and removes protected data on sign-out", async () => {
  const local = vi.spyOn(Storage.prototype, "setItem");
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Feeds" }));
  await screen.findByRole("button", { name: "production" });
  expect(local).not.toHaveBeenCalled();
  fireEvent.click(screen.getByRole("button", { name: "Sign out" }));
  expect(screen.queryByRole("button", { name: "production" })).toBeNull();
  expect(
    (await screen.findByLabelText("Short-lived bearer token")).getAttribute(
      "value",
    ),
  ).toBe("");
  expect(document.body.textContent).not.toContain("private-token");
});
it("loads feed details and saves with the returned ETag", async () => {
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Feeds" }));
  fireEvent.click(await screen.findByRole("button", { name: "production" }));
  const slug = await screen.findByLabelText("Feed slug");
  fireEvent.change(slug, { target: { value: "renamed" } });
  fireEvent.click(screen.getByRole("button", { name: "Save feed" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "PATCH")).toBe(true),
  );
  const call = calls.find((c) => c.init.method === "PATCH")!;
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"2"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    slug: "renamed",
    anonymous_read: false,
  });
});
it("edits a structured deny rule and saves a full policy", async () => {
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Feeds" }));
  fireEvent.click(await screen.findByRole("button", { name: "production" }));
  fireEvent.click(await screen.findByRole("button", { name: "Add rule" }));
  fireEvent.click(screen.getByLabelText("Deny matching writes"));
  fireEvent.change(screen.getByLabelText("namespace"), {
    target: { value: "private" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Save publish policy" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "PUT")).toBe(true),
  );
  const body = JSON.parse(
    calls.find((c) => c.init.method === "PUT")!.init.body as string,
  );
  expect(body.rules[0].deny).toBe(true);
  expect(body.rules[0].target_source_id).toBeUndefined();
  expect(body.rules[0].selector.namespace).toBe("private");
});
it("shows permission failures and does not render HTML from remote messages", async () => {
  vi.stubGlobal(
    "fetch",
    vi.fn(
      async () =>
        new Response(
          JSON.stringify({
            code: "forbidden",
            message: "<img src=x onerror=alert(1)>",
          }),
          { status: 403 },
        ),
    ),
  );
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Feeds" }));
  await screen.findByRole("alert");
  expect(screen.getByRole("alert").textContent).toContain("Permission denied");
  expect(document.querySelector("img")).toBeNull();
});
it("clears an authenticated session when the page is left", async () => {
  render(<App />);
  await login();
  fireEvent(window, new Event("pagehide"));
  expect(screen.getByLabelText("Short-lived bearer token")).toBeTruthy();
  expect(screen.queryByRole("button", { name: "Sign out" })).toBeNull();
});
it("creates a source using only supported configuration fields", async () => {
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Sources" }));
  fireEvent.click(screen.getByRole("button", { name: "Create source" }));
  fireEvent.change(screen.getByLabelText("Source name"), {
    target: { value: "release-store" },
  });
  fireEvent.change(screen.getByLabelText("Source kind"), {
    target: { value: "local" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Create source" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "POST")).toBe(true),
  );
  expect(
    JSON.parse(
      calls.find((c) => c.init.method === "POST")!.init.body as string,
    ),
  ).toEqual({ name: "release-store", kind: "local", format: "oci" });
});
it("attaches a source at an explicit read priority with feed revision protection", async () => {
  render(<App />);
  await login();
  fireEvent.click(screen.getByRole("button", { name: "Feeds" }));
  fireEvent.click(await screen.findByRole("button", { name: "production" }));
  fireEvent.click(await screen.findByText("Attach a source"));
  const sourceID = "00000000-0000-0000-0000-000000000002";
  fireEvent.change(screen.getByLabelText("Source UUID"), {
    target: { value: sourceID },
  });
  fireEvent.change(screen.getByLabelText("Read priority"), {
    target: { value: "50" },
  });
  fireEvent.click(screen.getByRole("button", { name: "Attach source" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "PUT")).toBe(true),
  );
  const call = calls.find((c) => c.init.method === "PUT")!;
  expect(call.url).toBe(`/api/v1/feeds/${feed.id}/sources/${sourceID}`);
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"2"');
  expect(JSON.parse(call.init.body as string)).toEqual({ priority: 50 });
});
