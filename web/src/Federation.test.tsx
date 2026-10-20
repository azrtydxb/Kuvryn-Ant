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
import { json, mockAPI, signInWithToken, type Call } from "./testkit";

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
const posts = (path: string) =>
  calls.filter((c) => c.init.method === "POST" && c.url === path);

const alpha = "11111111-2222-4333-8444-555555555555";
const beta = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee";
const none = {
  instance_id: alpha,
  state: "none",
  health: "disabled",
  listener_configured: true,
  sync: {},
  members: { active: 0, suspended: 0, removed: 0 },
  software_version: "2.0.0",
  protocol: { min: 1, max: 1 },
};
const active = {
  ...none,
  state: "active",
  role: "authority",
  health: "ok",
  federation_id: "6f1e2d3c-4b5a-4968-8776-655443322110",
  federation_name: "lab",
  authority_epoch: 1,
  ca_sha256: "a".repeat(64),
  membership: {
    generation: 3,
    hash: "sha256:" + "b".repeat(64),
    applied_at: "2026-10-18T10:00:00Z",
    age_seconds: 0,
    issued_at: "2026-10-18T10:00:00Z",
  },
  members: { active: 2, suspended: 0, removed: 0 },
};
const member = (id: string, name: string, extra = {}) => ({
  instance_id: id,
  name,
  status: "active",
  federation_url: `https://${name}.example:8444`,
  labels: { site: name },
  protocol: { min: 1, max: 1 },
  authority: false,
  self: false,
  membership_generation: 3,
  ...extra,
});

it("founds a federation and shows the recovery key once", async () => {
  let founded = false;
  install((url, init) => {
    if (url === "/api/v1/federation/status")
      return json(founded ? active : none);
    if (url === "/api/v1/federation/members")
      return json({
        items: founded
          ? [member(alpha, "alpha", { authority: true, self: true })]
          : [],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation" && init.method === "POST") {
      founded = true;
      return json(
        {
          status: active,
          recovery_key: "ant-recovery-v1:secret-material",
          recovery_public_key: "ed25519:pub",
          ca_sha256: "a".repeat(64),
        },
        201,
      );
    }
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await screen.findByText("Not federated");
  fireEvent.click(screen.getByRole("button", { name: "Found federation" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Found a federation",
  });
  const submit = within(dialog).getByRole("button", {
    name: "Found federation",
  });
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(within(dialog).getByLabelText("Federation name"), {
    target: { value: "lab" },
  });
  fireEvent.change(
    within(dialog).getByLabelText("This instance's member name"),
    { target: { value: "alpha" } },
  );
  fireEvent.click(submit);
  await waitFor(() => expect(posts("/api/v1/federation")).toHaveLength(1));
  expect(
    JSON.parse(posts("/api/v1/federation")[0].init.body as string),
  ).toEqual({ name: "lab", member_name: "alpha" });
  const shown = await screen.findByRole("dialog", {
    name: "Federation founded",
  });
  within(shown).getByText("ant-recovery-v1:secret-material");
  fireEvent.click(within(shown).getByRole("button", { name: "Done" }));
  await screen.findByText("Invite member");
  expect(screen.getAllByText("authority").length).toBeGreaterThan(0);
});

it("joins with the pinned CA fingerprint and invitation", async () => {
  let joined = false;
  install((url, init) => {
    if (url === "/api/v1/federation/status")
      return json(joined ? { ...active, role: "member" } : none);
    if (url === "/api/v1/federation/members")
      return json({
        items: joined
          ? [
              member(alpha, "alpha", { authority: true }),
              member(beta, "beta", { self: true }),
            ]
          : [],
      });
    if (url === "/api/v1/federation/join" && init.method === "POST") {
      joined = true;
      return json({ ...active, role: "member" });
    }
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  fireEvent.click(
    await screen.findByRole("button", { name: "Join federation" }),
  );
  const dialog = await screen.findByRole("dialog", {
    name: "Join a federation",
  });
  fireEvent.change(within(dialog).getByLabelText("Authority federation URL"), {
    target: { value: "https://alpha.example:8444" },
  });
  fireEvent.change(within(dialog).getByLabelText("CA fingerprint (SHA-256)"), {
    target: { value: "a".repeat(64) },
  });
  fireEvent.change(within(dialog).getByLabelText("Invitation"), {
    target: { value: beta + "." + "x".repeat(43) },
  });
  fireEvent.click(
    within(dialog).getByRole("button", { name: "Join federation" }),
  );
  await waitFor(() => expect(posts("/api/v1/federation/join")).toHaveLength(1));
  expect(
    JSON.parse(posts("/api/v1/federation/join")[0].init.body as string),
  ).toEqual({
    authority_url: "https://alpha.example:8444",
    ca_sha256: "a".repeat(64),
    invitation: beta + "." + "x".repeat(43),
  });
  await screen.findByRole("button", { name: "Leave" });
  await screen.findByText("this instance");
});

it("shows degraded federation and lets the authority remove a member with confirmation", async () => {
  install((url, init) => {
    if (url === "/api/v1/federation/status")
      return json({
        ...active,
        health: "degraded",
        sync: { last_error: "beta: connection refused" },
      });
    if (url === "/api/v1/federation/members")
      return json({
        items: [
          member(alpha, "alpha", { authority: true, self: true }),
          member(beta, "beta", { rtt_ms: 12 }),
        ],
      });
    if (url === "/api/v1/federation/invitations")
      return json({
        items: [
          {
            id: "00000000-0000-4000-8000-000000000009",
            name: "gamma",
            labels: {},
            expires_at: "2026-10-18T11:00:00Z",
            created_at: "2026-10-18T10:00:00Z",
            state: "pending",
          },
        ],
      });
    if (
      url.startsWith("/api/v1/federation/members/" + beta) &&
      init.method === "DELETE"
    )
      return json(member(beta, "beta", { status: "removed" }));
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await screen.findByText(/Last sync failed: beta: connection refused/);
  await screen.findByText("gamma");
  fireEvent.click(await screen.findByText("12 ms"));
  fireEvent.click(await screen.findByRole("button", { name: "Remove" }));
  const confirm = await screen.findByRole("dialog", { name: "Remove beta?" });
  fireEvent.click(
    within(confirm).getByRole("button", { name: "Remove member" }),
  );
  await waitFor(() =>
    expect(
      calls.find(
        (c) =>
          c.init.method === "DELETE" &&
          c.url === `/api/v1/federation/members/${beta}?confirm=true`,
      ),
    ).toBeTruthy(),
  );
});

const policyView = {
  status: {
    generation: 2,
    hash: "sha256:" + "c".repeat(64),
    applied_at: "2026-10-18T10:00:00Z",
    age_seconds: 0,
    staleness: "fresh",
    report: {
      roles: 1,
      grants: 2,
      principals: 1,
      revoked: 0,
      unresolved_feed_keys: ["lab/missing"],
      inert_admin_grants: 1,
      conflicts: [],
    },
  },
  document: {
    kind: "policy",
    federation_id: active.federation_id,
    federation_name: "lab",
    generation: 2,
    authority_epoch: 1,
    prev_hash: "sha256:" + "d".repeat(64),
    identity_domains: [
      { id: "idp", type: "oidc", issuer: "https://idp.example" },
    ],
    roles: [
      {
        key: "fed:readers",
        revision: 1,
        grants: [
          {
            kind: "feed",
            effect: "allow",
            feed_key: "lab/release",
            actions: ["read"],
          },
        ],
      },
    ],
    principal_bindings: [],
    group_bindings: [],
    service_accounts: [{ name: "ci", roles: ["fed:readers"] }],
    revocations: [],
    member_overrides: {},
    issued_at: "2026-10-18T10:00:00Z",
    issued_by: "admin",
  },
  has_draft: false,
  settings: { accept_admin_grants: false },
  history: [
    {
      generation: 2,
      hash: "sha256:" + "c".repeat(64),
      issued_at: "2026-10-18T10:00:00Z",
      issued_by: "admin",
      received_at: "2026-10-18T10:00:00Z",
    },
  ],
};

it("shows the applied policy and publishes the next generation from the authority", async () => {
  install((url, init) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url === "/api/v1/federation/members")
      return json({
        items: [member(alpha, "alpha", { authority: true, self: true })],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation/policy") return json(policyView);
    if (url === "/api/v1/federation/policy/draft")
      return json(
        { code: "not_found", message: "none", request_id: "r".repeat(16) },
        404,
      );
    if (url === "/api/v1/federation/feed-bindings") return json({ items: [] });
    if (url === "/api/v1/federation/principals") return json({ items: [] });
    if (url === "/api/v1/federation/policy/publish" && init.method === "POST")
      return json({
        generation: 3,
        hash: "sha256:" + "e".repeat(64),
        report: policyView.status.report,
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await openTab("Policy");
  await screen.findByText("Generation 2");
  await screen.findByText(/lab\/missing \(inert until bound\)/);
  screen.getByText("fed:readers");
  fireEvent.click(screen.getByRole("button", { name: "Edit policy" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Edit federation policy",
  });
  const publish = within(dialog).getByRole("button", { name: "Publish" });
  expect((publish as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(
    within(dialog).getByLabelText("Publish generation 3 to every member"),
  );
  fireEvent.click(publish);
  await waitFor(() =>
    expect(posts("/api/v1/federation/policy/publish")).toHaveLength(1),
  );
  const sent = JSON.parse(
    posts("/api/v1/federation/policy/publish")[0].init.body as string,
  );
  expect(sent.base_generation).toBe(2);
  expect(sent.confirm).toBe(true);
  expect(sent.policy.service_accounts).toEqual([
    { name: "ci", roles: ["fed:readers"] },
  ]);
});

it("shows policy alerts on a member and adopts a conflicting local principal", async () => {
  const local = "00000000-0000-4000-8000-0000000000aa";
  install((url, init) => {
    if (url === "/api/v1/federation/status")
      return json({
        ...active,
        role: "member",
        health: "degraded",
        alerts: [
          {
            code: "policy_stale",
            severity: "warning",
            message: "federation policy not confirmed current for 20m0s",
          },
        ],
      });
    if (url === "/api/v1/federation/members")
      return json({ items: [member(alpha, "alpha", { authority: true })] });
    if (url === "/api/v1/federation/policy")
      return json({
        ...policyView,
        status: {
          ...policyView.status,
          staleness: "warning",
          age_seconds: 1200,
        },
      });
    if (url === "/api/v1/federation/feed-bindings") return json({ items: [] });
    if (url === "/api/v1/federation/principals")
      return json({
        items: [
          {
            id: local,
            kind: "user",
            issuer: "idp",
            subject: "dave",
            origin: "local",
            disabled: false,
            roles: [],
            local_roles: [],
            conflict: "conflict_local_principal",
          },
        ],
      });
    if (
      url === `/api/v1/federation/principals/${local}/adopt` &&
      init.method === "POST"
    )
      return json(policyView.status.report);
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await openTab("Policy");
  await screen.findByText(/not confirmed current for 20m0s/);
  expect(screen.queryByRole("button", { name: "Edit policy" })).toBeNull();
  fireEvent.click(await screen.findByRole("button", { name: "Adopt" }));
  await waitFor(() =>
    expect(posts(`/api/v1/federation/principals/${local}/adopt`)).toHaveLength(
      1,
    ),
  );
});

it("toggles the package search scope to the federation and reports partial results", async () => {
  install((url) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url.startsWith("/api/v1/packages"))
      return json({ items: [], page: { has_more: false }, total: 0 });
    if (url.startsWith("/api/v1/federation/search"))
      return json({
        items: [
          {
            member: alpha,
            member_name: "alpha",
            feed_key: "lab/release",
            feed_slug: "release",
            format: "raw",
            coordinate: "pkg-a",
            namespace: "public",
            origin: "local",
            home: true,
            api_url: "https://alpha.example",
            copies: [
              {
                member: alpha,
                member_name: "alpha",
                origin: "local",
                home: true,
                api_url: "https://alpha.example",
              },
              {
                member: beta,
                member_name: "beta",
                origin: "replica",
                home: false,
                api_url: "https://beta.example",
              },
            ],
          },
        ],
        members: [
          { id: alpha, name: "alpha", status: "ok", took_ms: 3, hits: 1 },
          {
            id: beta,
            name: "beta",
            status: "timeout",
            code: "deadline",
            took_ms: 700,
            hits: 0,
          },
        ],
        partial: true,
        page: { has_more: false },
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Packages");
  const scope = await screen
    .findByRole("radio", { name: "Federation" })
    .catch(() => screen.findByText("Federation"));
  fireEvent.click(scope);
  await screen.findByText("Partial results");
  await screen.findByText("pkg-a");
  expect(
    screen.getByText("beta: timeout (deadline) · 0 · 700 ms"),
  ).toBeTruthy();
  expect(screen.getByText("beta (replica)")).toBeTruthy();
  expect(calls.some((c) => c.url.startsWith("/api/v1/federation/search"))).toBe(
    true,
  );
});

it("shows federated feeds, rule compliance and starts a planned home transfer", async () => {
  const ruleID = "99999999-8888-4777-8666-555555555555";
  install((url, init) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url === "/api/v1/federation/members")
      return json({
        items: [
          member(alpha, "alpha", { authority: true, self: true }),
          member(beta, "beta"),
        ],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation/topology")
      return json({
        generation: 4,
        authority: true,
        feeds: [
          {
            key: "lab/release",
            home: alpha,
            home_name: "alpha",
            descriptor: { format: "npm", anonymous_read: false },
            revision: 1,
            local_role: "home",
            frozen: false,
          },
        ],
        rules: [
          {
            id: ruleID,
            feed_key: "lab/release",
            targets: { labels: { site: "beta" } },
            mode: "continuous",
            interval_seconds: 300,
            include_cached: true,
            filters: {},
            deletion_policy: "ignore",
            destination_read_only: true,
            create_destination_feed: true,
            adopt_existing: true,
            max_lag_seconds: 900,
            revision: 1,
          },
        ],
      });
    if (url === "/api/v1/federation/replication/status")
      return json({ items: [], quarantines: [] });
    if (url === `/api/v1/federation/replication/rules/${ruleID}/compliance`)
      return json({
        rule: { id: ruleID, feed_key: "lab/release", revision: 1 },
        home: alpha,
        members: [
          {
            rule_id: ruleID,
            feed_key: "lab/release",
            member_id: beta,
            member_name: "beta",
            state: "in_sync",
            adopted: true,
            checkpoint: 42,
            consecutive_failures: 0,
          },
        ],
        summary: { in_sync: 1 },
      });
    if (
      url === "/api/v1/federation/feeds/lab/release/transfer" &&
      init.method === "POST"
    )
      return json(
        {
          id: ruleID,
          from: alpha,
          to: beta,
          mode: "planned",
          state: "freeze",
          head: 0,
          checkpoint: 0,
          baseline: 0,
          started_at: "2026-10-19T10:00:00Z",
          updated_at: "2026-10-19T10:00:00Z",
        },
        202,
      );
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Replication");
  await openTab("Federation rules");
  await screen.findByText("Federated feeds");
  await waitFor(() =>
    expect(screen.getAllByText("site=beta").length).toBeGreaterThan(0),
  );
  fireEvent.click(await screen.findByRole("button", { name: "Compliance" }));
  const comp = await screen.findByRole("dialog", { name: "Rule compliance" });
  await within(comp).findByText("in_sync");
  fireEvent.click(within(comp).getByRole("button", { name: "Done" }));
  fireEvent.click(screen.getByRole("button", { name: "Transfer home" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Transfer the home of lab/release",
  });
  const start = within(dialog).getByRole("button", { name: "Start transfer" });
  fireEvent.change(within(dialog).getByLabelText("New home"), {
    target: { value: beta },
  });
  expect((start as HTMLButtonElement).disabled).toBe(true);
  fireEvent.click(
    within(dialog).getByLabelText("Move the home of lab/release"),
  );
  fireEvent.click(start);
  await waitFor(() =>
    expect(posts("/api/v1/federation/feeds/lab/release/transfer")).toHaveLength(
      1,
    ),
  );
  const sent = JSON.parse(
    posts("/api/v1/federation/feeds/lab/release/transfer")[0].init
      .body as string,
  );
  expect(sent).toEqual({
    to_member_id: beta,
    mode: "planned",
    confirm: true,
    accept_data_loss: false,
  });
});

it("explains read routing and publishes routing settings", async () => {
  install((url, init) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url === "/api/v1/federation/members")
      return json({
        items: [
          member(alpha, "alpha", { authority: true, self: true }),
          member(beta, "beta"),
        ],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation/topology")
      return json({
        generation: 5,
        authority: true,
        feeds: [
          {
            key: "lab/release",
            home: beta,
            home_name: "beta",
            descriptor: { format: "npm", anonymous_read: false },
            revision: 2,
            routing: { mode: "proxy", blob_urls: true, cache: false },
            local_role: "unbound",
            frozen: false,
          },
        ],
        rules: [],
      });
    if (url === "/api/v1/federation/replication/status")
      return json({ items: [], quarantines: [] });
    if (url === "/api/v1/federation/routes/lab/release")
      return json({
        feed_key: "lab/release",
        format: "npm",
        home: beta,
        local_role: "unbound",
        routing: { mode: "proxy", blob_urls: true, cache: false },
        decision: "proxy",
        reason: "a local miss is proxied",
        candidates: [
          {
            member_id: beta,
            name: "beta",
            role: "home",
            api_url: "https://beta.example",
            circuit: "closed",
            consecutive_failures: 0,
            rtt_ms: 12.4,
            preference: 0,
            eligible: true,
            order: 1,
            reason: "home: authoritative holder, tried after replicas",
          },
        ],
      });
    if (
      url === "/api/v1/federation/feeds/lab/release/routing" &&
      init.method === "PUT"
    )
      return json({
        key: "lab/release",
        home: beta,
        descriptor: { format: "npm", anonymous_read: false },
        revision: 3,
        routing: { mode: "redirect", blob_urls: false, cache: false },
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Replication");
  await openTab("Federation rules");
  await screen.findByText("proxy · blob URLs");
  fireEvent.click(screen.getByRole("button", { name: "Explain" }));
  const explain = await screen.findByRole("dialog", {
    name: "Read routing of lab/release",
  });
  await within(explain).findByText("Proxied to another member");
  expect(within(explain).getByText("12 ms")).toBeTruthy();
  fireEvent.click(within(explain).getByRole("button", { name: "Done" }));
  fireEvent.click(screen.getByRole("button", { name: "Edit" }));
  const edit = await screen.findByRole("dialog", {
    name: "Routing of lab/release",
  });
  fireEvent.change(within(edit).getByLabelText("Mode"), {
    target: { value: "redirect" },
  });
  fireEvent.click(
    within(edit).getByRole("button", { name: "Publish routing" }),
  );
  await waitFor(() =>
    expect(
      calls.filter(
        (c) =>
          c.init.method === "PUT" &&
          c.url === "/api/v1/federation/feeds/lab/release/routing",
      ),
    ).toHaveLength(1),
  );
  const put = calls.find(
    (c) => c.url === "/api/v1/federation/feeds/lab/release/routing",
  )!;
  expect(JSON.parse(put.init.body as string)).toEqual({
    mode: "redirect",
    blob_urls: false,
    cache: false,
  });
});

it("shows authority epochs, hands the authority over with a typed confirmation and reads the federated audit", async () => {
  let handedOver = false;
  install((url, init) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url === "/api/v1/federation/members")
      return json({
        items: [
          member(alpha, "alpha", { authority: true, self: true }),
          member(beta, "beta"),
        ],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation/authority")
      return json({
        role: "authority",
        epoch: handedOver ? 2 : 1,
        authority_instance_id: alpha,
        authority_name: "alpha",
        key_fingerprint: "a".repeat(16),
        recovery_key_fingerprint: "b".repeat(16),
        epochs: [
          {
            epoch: 1,
            instance_id: alpha,
            name: "alpha",
            certified_by: "recovery",
            from_generation: 1,
            key_fingerprint: "a".repeat(16),
            applied_at: "2026-10-18T10:00:00Z",
          },
        ],
        recovery: { due: true },
        pending_authority_key: false,
      });
    if (
      url === "/api/v1/federation/authority/handover" &&
      init.method === "POST"
    ) {
      handedOver = true;
      return json({ status: active, epoch: 2, applied: true });
    }
    if (url.startsWith("/api/v1/federation/audit"))
      return json({
        items: [
          {
            member: alpha,
            member_name: "alpha",
            id: 7,
            created_at: "2026-10-18T10:00:00Z",
            action: "federation.member.added",
            result: "success",
            details: { name: "beta" },
          },
        ],
        members: [
          {
            id: alpha,
            name: "alpha",
            status: "ok",
            took_ms: 3,
            events: 1,
          },
          {
            id: beta,
            name: "beta",
            status: "forbidden",
            code: "audit_read_denied",
            took_ms: 5,
            events: 0,
          },
        ],
        partial: true,
        page: { has_more: false },
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await openTab("Authority");
  await screen.findByText("Recovery key not proven recently");
  expect(screen.getAllByText("recovery key").length).toBeGreaterThan(0);
  // Federated audit: the member that denies the caller is reported.
  await openTab("Audit");
  await screen.findByText("federation.member.added");
  screen.getByText("beta: forbidden (audit_read_denied) · 0 · 5 ms");
  screen.getByText("Partial results");
  // Handover needs the member name typed.
  await openTab("Authority");
  fireEvent.click(await screen.findByRole("button", { name: "Hand over" }));
  const dialog = await screen.findByRole("dialog", {
    name: "Hand over the authority",
  });
  const submit = within(dialog).getByRole("button", { name: "Hand over" });
  expect((submit as HTMLButtonElement).disabled).toBe(true);
  fireEvent.change(within(dialog).getByLabelText("Type beta to confirm"), {
    target: { value: "beta" },
  });
  fireEvent.click(submit);
  await waitFor(() =>
    expect(posts("/api/v1/federation/authority/handover")).toHaveLength(1),
  );
  expect(
    JSON.parse(
      posts("/api/v1/federation/authority/handover")[0].init.body as string,
    ),
  ).toEqual({ to_member_id: beta, confirm: true });
});

async function openTab(name: string) {
  fireEvent.click(
    await screen.findByRole("tab", { name: new RegExp(`^${name}`) }),
  );
}

it("replaced the Federation replication tab with a link to Replication", async () => {
  install((url) => {
    if (url === "/api/v1/federation/status") return json(active);
    if (url === "/api/v1/federation/members")
      return json({
        items: [member(alpha, "alpha", { authority: true, self: true })],
      });
    if (url === "/api/v1/federation/invitations") return json({ items: [] });
    if (url === "/api/v1/federation/topology")
      return json({ generation: 1, authority: true, feeds: [], rules: [] });
    if (url === "/api/v1/federation/replication/status")
      return json({ items: [], quarantines: [] });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Federation");
  await screen.findByRole("tab", { name: /^Members/ });
  expect(screen.queryByRole("tab", { name: /^Replication/ })).toBeNull();
  fireEvent.click(
    await screen.findByRole("button", {
      name: /Replication → Federation rules/,
    }),
  );
  await screen.findByText("Federated feeds");
  expect(
    screen
      .getByRole("tab", { name: /^Federation rules/ })
      .getAttribute("aria-selected"),
  ).toBe("true");
});
