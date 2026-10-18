// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  waitFor,
} from "@testing-library/react";
import { Client } from "./api";
import { FeedAccess, PersonalTokensPane } from "./ClientAuth";
import { ids, json, mockAPI, type Call } from "./testkit";
import { ToastProvider } from "./ui";

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
const wrap = (node: React.ReactNode) =>
  render(<ToastProvider>{node}</ToastProvider>);
const writes = (m: string) => calls.filter((c) => c.init.method === m);

const tokenID = "00000000-0000-0000-0000-0000000000aa";
const secret = "antp_SECRETSECRETSECRETSECRETSECRETSECRETSECRE";
const listed = {
  id: tokenID,
  name: "laptop",
  scopes: [{ actions: ["read"] }],
  created_at: "2026-01-01T00:00:00Z",
  expires_at: "2099-01-01T00:00:00Z",
};

it("creates a personal access token, shows the secret once and revokes it", async () => {
  let revoked = false;
  install((url, init) => {
    const path = url.split("?")[0];
    if (path === "/api/v1/auth/personal-access-tokens") {
      if (init.method === "POST")
        return json({ ...listed, name: "ci", token: secret }, 201);
      return json({
        items: revoked
          ? [{ ...listed, revoked_at: "2026-02-01T00:00:00Z" }]
          : [listed],
      });
    }
    if (path === `/api/v1/auth/personal-access-tokens/${tokenID}`) {
      revoked = true;
      return new Response(null, { status: 204 });
    }
    return undefined;
  });
  wrap(<PersonalTokensPane client={new Client("t")} />);
  await screen.findByText("laptop");
  expect(document.body.textContent).not.toContain(secret);

  fireEvent.click(screen.getByRole("button", { name: "Create token" }));
  fireEvent.change(await screen.findByLabelText("Name"), {
    target: { value: "ci" },
  });
  fireEvent.click(screen.getByLabelText("publish"));
  fireEvent.click(screen.getByRole("button", { name: "Create token once" }));
  const field = (await screen.findByLabelText(
    "New token secret — copy now",
  )) as HTMLInputElement;
  expect(field.value).toBe(secret);
  const body = JSON.parse(String(writes("POST")[0].init.body));
  expect(body).toEqual({
    name: "ci",
    scopes: [{ actions: ["read", "publish"] }],
    expires_in_days: 90,
  });

  fireEvent.click(screen.getByRole("button", { name: "Done" }));
  await waitFor(() => expect(document.body.textContent).not.toContain(secret));

  fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
  fireEvent.click(await screen.findByRole("button", { name: "Revoke token" }));
  await waitFor(() => expect(writes("DELETE")).toHaveLength(1));
  await screen.findByText("Revoked");
});

it("edits a feed's client access policy and falls back to the default", async () => {
  let inherited = true;
  install((url, init) => {
    const path = url.split("?")[0];
    if (path === `/api/v1/feeds/${ids.feed}/auth-policy`) {
      if (init.method === "PUT") {
        inherited = false;
        return json({
          feed_id: ids.feed,
          ...JSON.parse(String(init.body)),
          inherited: false,
          revision: 1,
        });
      }
      if (init.method === "DELETE") {
        inherited = true;
        return new Response(null, { status: 204 });
      }
      return json({
        feed_id: ids.feed,
        methods: ["token", "pat"],
        anonymous_network_group_ids: [],
        inherited,
        revision: 1,
      });
    }
    if (path === "/api/v1/auth/network-groups")
      return json({
        items: [
          {
            id: "00000000-0000-0000-0000-0000000000bb",
            name: "cluster",
            cidrs: ["10.0.0.0/8"],
            revision: 1,
          },
        ],
      });
    return undefined;
  });
  wrap(<FeedAccess client={new Client("t")} feedId={ids.feed} />);
  await screen.findByText("Inherited from the instance default.");
  expect(
    screen.queryByRole("button", { name: "Use instance default" }),
  ).toBeNull();
  fireEvent.click(screen.getByLabelText("Anonymous"));
  fireEvent.click(await screen.findByLabelText("cluster"));
  fireEvent.click(screen.getByLabelText("Directory password"));
  fireEvent.click(screen.getByRole("button", { name: "Save access" }));
  await waitFor(() => expect(writes("PUT")).toHaveLength(1));
  expect(JSON.parse(String(writes("PUT")[0].init.body))).toEqual({
    methods: ["anonymous", "token", "basic-directory", "pat"],
    anonymous_network_group_ids: ["00000000-0000-0000-0000-0000000000bb"],
  });
  fireEvent.click(
    await screen.findByRole("button", { name: "Use instance default" }),
  );
  await waitFor(() => expect(writes("DELETE")).toHaveLength(1));
});
