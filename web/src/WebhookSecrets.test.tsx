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
import { Webhooks } from "./Webhooks";
import { ToastProvider } from "./ui";
import { Client } from "./api";
import { json, mockAPI, type Call } from "./testkit";

const prod = {
  id: "11111111-1111-1111-1111-111111111111",
  name: "production",
  description: "Production receivers",
  created_at: "2026-01-01T00:00:00Z",
  revision: 3,
  used_by_count: 2,
  used_by: [
    {
      id: "aaaaaaaa-0000-0000-0000-000000000001",
      name: "deploys",
      endpoint: "https://hooks.example.com/ant/deploys",
    },
    {
      id: "aaaaaaaa-0000-0000-0000-000000000002",
      name: "releases",
      endpoint: "https://hooks.example.com/ant/releases",
    },
  ],
};
const hook = {
  id: "aaaaaaaa-0000-0000-0000-000000000001",
  name: "deploys",
  endpoint: "https://hooks.example.com/ant/deploys",
  secret_id: prod.id,
  secret_name: "production",
  enabled: true,
  revision: 1,
  created_at: "2026-01-02T00:00:00Z",
};
const created = {
  ...prod,
  id: "22222222-2222-2222-2222-222222222222",
  name: "fresh",
  revision: 1,
  used_by_count: 0,
  used_by: [],
  value: "whsec_shown-once-value",
};

let calls: Call[];
let secrets = [prod];
function install() {
  const api = mockAPI((url, init) => {
    const path = url.split("?")[0];
    const method = init.method ?? "GET";
    if (path === "/api/v1/webhooks" && method === "GET")
      return json({ items: [hook] });
    if (path === "/api/v1/webhook-secrets" && method === "GET")
      return json({ items: secrets });
    if (path === "/api/v1/webhook-secrets" && method === "POST") {
      secrets = [...secrets, created];
      return json(created, 201, '"1"');
    }
    if (path === `/api/v1/webhook-secrets/${prod.id}/rotate`)
      return json({ ...prod, value: "whsec_rotated", revision: 4 }, 200, '"4"');
    return undefined;
  });
  calls = api.calls;
  vi.stubGlobal("fetch", api.fetcher);
}
beforeEach(() => {
  secrets = [prod];
  install();
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
const renderHooks = () =>
  render(
    <ToastProvider>
      <Webhooks client={new Client("token")} initialTab="outgoing" />
    </ToastProvider>,
  );
const writes = (method: string) =>
  calls.filter((c) => c.init.method === method);

it("lists signing secrets under Access with usage and actions", async () => {
  render(
    <ToastProvider>
      <Identity client={new Client("token")} initialTab="secrets" />
    </ToastProvider>,
  );
  const row = (await screen.findByText("production")).closest("tr")!;
  expect(within(row).getByText("2")).toBeTruthy();
  expect(within(row).getByRole("button", { name: /Rotate/ })).toBeTruthy();
  expect(
    screen.getByRole("button", { name: "Create signing secret" }),
  ).toBeTruthy();
});

it("validates the name and the full URL inline and creates with both", async () => {
  renderHooks();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create webhook" }),
  );
  const dialog = await screen.findByRole("dialog", { name: "Create webhook" });
  const create = within(dialog).getByRole("button", { name: "Create webhook" });
  const name = await within(dialog).findByLabelText("Name");
  const url = within(dialog).getByLabelText("Webhook URL");
  // There is no slug, base URL or preview any more.
  expect(within(dialog).queryByTestId("url-preview")).toBeNull();
  fireEvent.change(name, { target: { value: "Build events" } });
  fireEvent.change(url, { target: { value: "http://hooks.example.com/x" } });
  expect(within(dialog).getByText("Use https.")).toBeTruthy();
  fireEvent.change(url, {
    target: { value: "https://u:p@hooks.example.com/x" },
  });
  expect(within(dialog).getByText(/No credentials/)).toBeTruthy();
  fireEvent.change(url, {
    target: { value: "https://hooks.example.com/ant/build" },
  });
  expect(create).toHaveProperty("disabled", true); // no signing secret yet
  fireEvent.change(within(dialog).getByLabelText("Signing secret"), {
    target: { value: prod.id },
  });
  expect(create).toHaveProperty("disabled", false);
  fireEvent.click(create);
  await waitFor(() => expect(writes("POST")).toHaveLength(1));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    name: "Build events",
    endpoint: "https://hooks.example.com/ant/build",
    secret_id: prod.id,
  });
});

it("creates a signing secret from the dropdown with the shared modal and selects it", async () => {
  renderHooks();
  fireEvent.click(
    await screen.findByRole("button", { name: "Create webhook" }),
  );
  const dialog = await screen.findByRole("dialog", { name: "Create webhook" });
  const select = (await within(dialog).findByLabelText(
    "Signing secret",
  )) as HTMLSelectElement;
  expect([...select.options].at(-1)!.textContent).toBe(
    "+ Create new signing secret…",
  );
  fireEvent.change(select, { target: { value: "__new__" } });
  const modal = await screen.findByRole("dialog", {
    name: "Create signing secret",
  });
  fireEvent.change(within(modal).getByLabelText("Name"), {
    target: { value: "fresh" },
  });
  fireEvent.click(within(modal).getByRole("button", { name: "Create secret" }));
  // The value is shown once with a store-it-now warning.
  expect((await screen.findByTestId("secret-value")).textContent).toContain(
    "whsec_shown-once-value",
  );
  expect(screen.getByText("Store it now")).toBeTruthy();
  fireEvent.click(screen.getByRole("button", { name: "I have stored it" }));
  await waitFor(() =>
    expect(
      screen.queryByRole("dialog", { name: /signing secret/i }),
    ).toBeNull(),
  );
  const picked = within(
    screen.getByRole("dialog", { name: "Create webhook" }),
  ).getByLabelText("Signing secret") as HTMLSelectElement;
  await waitFor(() => expect(picked.value).toBe(created.id));
  expect(JSON.parse(writes("POST")[0].init.body as string)).toEqual({
    name: "fresh",
  });
});

it("warns which other webhooks share a secret before rotating it", async () => {
  renderHooks();
  fireEvent.click(await screen.findByRole("button", { name: /deploys/ }));
  const drawer = await screen.findByRole("dialog");
  fireEvent.click(
    await within(drawer).findByRole("button", { name: "Rotate secret" }),
  );
  const modal = await screen.findByRole("dialog", {
    name: "Rotate signing secret",
  });
  expect(within(modal).getByText(/Also used by: releases/)).toBeTruthy();
  expect(within(modal).queryByText(/Also used by:.*deploys/)).toBeNull();
  fireEvent.click(within(modal).getByRole("button", { name: "Rotate secret" }));
  expect((await screen.findByTestId("secret-value")).textContent).toContain(
    "whsec_rotated",
  );
  const body = JSON.parse(writes("POST")[0].init.body as string);
  expect(body).toEqual({ grace_seconds: 0 });
});
