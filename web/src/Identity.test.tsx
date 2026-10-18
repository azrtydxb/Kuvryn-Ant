// @vitest-environment jsdom
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { Identity } from "./Identity";
import { Client } from "./api";
const principal = {
  id: "00000000-0000-0000-0000-000000000001",
  subject: "operator",
  kind: "user",
  disabled: false,
  revision: 7,
  role_ids: [],
};
const service = {
  ...principal,
  id: "00000000-0000-0000-0000-000000000002",
  subject: "automation",
  kind: "service",
};
const role = {
  id: "00000000-0000-0000-0000-000000000003",
  name: "readers",
  revision: 3,
  grants: [],
};
let calls: { url: string; init: RequestInit }[];
beforeEach(() => {
  calls = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) => {
      calls.push({ url, init });
      return new Response(
        JSON.stringify(
          url.includes("/principals?")
            ? { items: [principal, service] }
            : url.includes("/roles?")
              ? { items: [role] }
              : {},
        ),
      );
    }),
  );
});
afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});
it("binds exact issuer and immutable subject to an explicit existing user", async () => {
  render(<Identity client={new Client("token")} />);
  fireEvent.click(await screen.findByText("Bind provider identity"));
  fireEvent.change(screen.getByLabelText("Provider issuer / identifier"), {
    target: { value: "https://issuer.example" },
  });
  fireEvent.change(screen.getByLabelText("Immutable provider subject"), {
    target: { value: "immutable-subject" },
  });
  fireEvent.click(
    screen.getByRole("button", { name: "Create identity binding" }),
  );
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "POST")).toBe(true),
  );
  expect(
    JSON.parse(
      calls.find((c) => c.init.method === "POST")!.init.body as string,
    ),
  ).toEqual({
    issuer: "https://issuer.example",
    subject: "immutable-subject",
    principal_id: principal.id,
  });
  expect(screen.getAllByText("Issue scoped service token")).toHaveLength(1);
});
it("replaces memberships using the listed principal revision", async () => {
  render(<Identity client={new Client("token")} />);
  await screen.findByRole("heading", { name: "operator" });
  fireEvent.click(screen.getAllByText("Role memberships")[0]);
  fireEvent.change(screen.getAllByLabelText("Role UUIDs")[0], {
    target: { value: role.id },
  });
  fireEvent.click(
    screen.getAllByRole("button", { name: "Replace role memberships" })[0],
  );
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "PUT")).toBe(true),
  );
  const call = calls.find((c) => c.init.method === "PUT")!;
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"7"');
  expect(JSON.parse(call.init.body as string)).toEqual({ role_ids: [role.id] });
});
it("edits structured grants with the role revision and no unsupported scope fields", async () => {
  render(<Identity client={new Client("token")} />);
  fireEvent.click(screen.getByRole("button", { name: "Roles" }));
  fireEvent.click(await screen.findByText("Display and edit grants"));
  fireEvent.click(screen.getByRole("button", { name: "Add grant" }));
  fireEvent.change(screen.getByLabelText("Resource kind"), {
    target: { value: "instance" },
  });
  fireEvent.click(screen.getByLabelText("admin"));
  fireEvent.click(screen.getByRole("button", { name: "Replace role grants" }));
  await waitFor(() =>
    expect(calls.some((c) => c.init.method === "PUT")).toBe(true),
  );
  const call = calls.find((c) => c.init.method === "PUT")!;
  expect(new Headers(call.init.headers).get("If-Match")).toBe('"3"');
  expect(JSON.parse(call.init.body as string)).toEqual({
    grants: [{ kind: "instance", effect: "allow", actions: ["read", "admin"] }],
  });
});
it("retains issuance failure feedback and requires a distinct new attempt", async () => {
  const original = globalThis.fetch;
  vi.stubGlobal(
    "fetch",
    vi.fn(async (url: string, init: RequestInit) =>
      init.method === "POST"
        ? new Response("{}", { status: 503 })
        : original(url, init),
    ),
  );
  render(<Identity client={new Client("token")} />);
  fireEvent.click(await screen.findByText("Issue scoped service token"));
  fireEvent.change(screen.getByLabelText("Token expiry (local time)"), {
    target: {
      value: new Date(Date.now() + 86400000).toISOString().slice(0, 16),
    },
  });
  fireEvent.click(screen.getByRole("button", { name: "Issue token once" }));
  await screen.findByRole("alert");
  expect(screen.getByRole("alert").textContent).toContain(
    "Service unavailable",
  );
  expect(screen.queryByRole("button", { name: "Issue token once" })).toBeNull();
  expect(
    screen.getByRole("button", { name: "Start a separate token issuance" }),
  ).toBeTruthy();
});
