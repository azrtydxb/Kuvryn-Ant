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
import { DropdownMenu, Table } from "./ds/bundle";
import { ids, json, mockAPI, signInWithToken, source } from "./testkit";
import { shortId } from "./ui";

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const install = (...a: Parameters<typeof mockAPI>) =>
  vi.stubGlobal("fetch", mockAPI(...a).fetcher);
const nav = (name: string) =>
  fireEvent.click(
    within(
      screen.getByRole("navigation", { name: "Main navigation" }),
    ).getByRole("button", { name }),
  );

it("renders row action menus in a portal outside the table's overflow container", () => {
  const onSelect = vi.fn();
  const onRow = vi.fn();
  render(
    <Table
      onRowClick={onRow}
      columns={[
        { key: "name", label: "Name" },
        {
          key: "actions",
          label: "",
          render: () => (
            <DropdownMenu
              align="end"
              trigger={<button>Open</button>}
              items={[{ label: "Do it", onSelect }]}
            />
          ),
        },
      ]}
      rows={[{ id: "1", name: "one" }]}
    />,
  );
  // Empty header gets an accessible name.
  expect(screen.getByRole("columnheader", { name: "Actions" })).toBeTruthy();
  fireEvent.click(screen.getByText("Open"));
  const menu = screen.getByRole("menu");
  expect(menu.closest(".az-table-wrap")).toBeNull();
  expect(menu.parentElement).toBe(document.body);
  fireEvent.click(screen.getByRole("menuitem", { name: "Do it" }));
  expect(onSelect).toHaveBeenCalled();
  // Selecting an item must not also activate the row.
  expect(onRow).not.toHaveBeenCalled();
  expect(screen.queryByRole("menu")).toBeNull();
});

it("closes a portal menu on outside click and Escape", () => {
  render(
    <DropdownMenu trigger={<button>Open</button>} items={[{ label: "A" }]} />,
  );
  fireEvent.click(screen.getByText("Open"));
  fireEvent.mouseDown(document.body);
  expect(screen.queryByRole("menu")).toBeNull();
  fireEvent.click(screen.getByText("Open"));
  fireEvent.keyDown(document, { key: "Escape" });
  expect(screen.queryByRole("menu")).toBeNull();
});

it("does not offer replica sources and continues a new remote source to its upstream configuration", async () => {
  const mock = mockAPI((url, init) => {
    const made = { ...source, id: ids.source, name: "up", kind: "remote" };
    if (
      url === `/api/v1/sources/${ids.source}/remote-config` &&
      init.method === "PUT"
    )
      return json({ revision: 2 });
    if (url === "/api/v1/sources" && init.method === "POST")
      return json(made, 201, '"1"');
    if (url.startsWith("/api/v1/sources?"))
      return json({ items: [made], page: { has_more: false } });
    return undefined;
  });
  vi.stubGlobal("fetch", mock.fetcher);
  render(<App />);
  await signInWithToken();
  nav("Sources");
  fireEvent.click(await screen.findByRole("button", { name: "Create source" }));
  const dialog = screen.getByRole("dialog", { name: "Create source" });
  const kinds = within(dialog)
    .getAllByRole("option")
    .map((o) => o.textContent);
  expect(kinds.some((k) => k?.includes("replica"))).toBe(false);
  fireEvent.change(within(dialog).getByLabelText("Source kind"), {
    target: { value: "remote" },
  });
  expect(dialog.textContent).not.toContain("API does not report");
  // Presets for the chosen format, plus a custom tile; picking one prefills.
  expect(
    within(dialog).getByRole("button", { name: "Custom upstream" }),
  ).toBeTruthy();
  expect(within(dialog).queryByRole("button", { name: "npm" })).toBeNull();
  fireEvent.click(within(dialog).getByRole("button", { name: "Docker Hub" }));
  expect(
    (within(dialog).getByLabelText("Source name") as HTMLInputElement).value,
  ).toBe("dockerhub-remote");
  expect(
    (
      within(dialog).getByLabelText(
        "Upstream HTTPS base URL",
      ) as HTMLInputElement
    ).value,
  ).toBe("https://registry-1.docker.io");
  expect(
    (
      within(dialog).getByLabelText(
        "Allowed redirect origins",
      ) as HTMLTextAreaElement
    ).value,
  ).toContain("https://auth.docker.io");
  fireEvent.change(within(dialog).getByLabelText("Source name"), {
    target: { value: "up" },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Create source" }),
  );
  const tab = await screen.findByRole("tab", { name: "Upstream" });
  await waitFor(() => expect(tab.getAttribute("aria-selected")).toBe("true"));
  const put = mock.calls.find(
    (c) => c.url.endsWith("/remote-config") && c.init.method === "PUT",
  );
  expect(put).toBeDefined();
  const body = JSON.parse(String(put!.init.body));
  expect(body.origin).toBe("https://registry-1.docker.io");
  expect(body.redirect_origins).toContain(
    "https://production.cloudflare.docker.com",
  );
  expect(JSON.stringify(body)).not.toMatch(/authorization|api_key|credential/i);
});

it("links from the create-source dialog to Replication for replicas", async () => {
  install();
  render(<App />);
  await signInWithToken();
  nav("Sources");
  fireEvent.click(await screen.findByRole("button", { name: "Create source" }));
  const dialog = screen.getByRole("dialog", { name: "Create source" });
  fireEvent.click(within(dialog).getByRole("button", { name: "Replication" }));
  await screen.findByRole("heading", { name: "Replication" });
});

it("shows names instead of raw IDs in the audit log", async () => {
  install((url) =>
    url.startsWith("/api/v1/audit?")
      ? json({
          items: [
            {
              id: "e1",
              action: "source.create",
              result: "success",
              created_at: "2026-10-19T10:00:00Z",
              resource_id: ids.source,
            },
          ],
          page: { has_more: false },
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Audit log");
  await screen.findByText("source.create");
  await screen.findByText("oci-local");
  expect(document.body.textContent).not.toContain(ids.source);
});

it("shortens long identifiers", () => {
  expect(shortId("sha256:" + "ab".repeat(32), 12)).toBe("abababababab…");
  expect(shortId("short")).toBe("short");
});
