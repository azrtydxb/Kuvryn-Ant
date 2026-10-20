// @vitest-environment jsdom
import { afterEach, expect, it, vi } from "vitest";
import {
  cleanup,
  fireEvent,
  render,
  screen,
  within,
} from "@testing-library/react";
import App from "./App";
import { ids, json, mockAPI, page, signInWithToken } from "./testkit";
import {
  orderTags,
  shortChecksum,
  shortVersion,
  isDigestVersion,
} from "./versions";
import { extraNamespace, packageName, pullCommand } from "./Packages";

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

const digest = "a".repeat(12) + "b".repeat(52);
const index = "c".repeat(64);

it("shortens digests to sha256: plus 12 characters", () => {
  expect(isDigestVersion(digest)).toBe(true);
  expect(shortVersion(digest)).toBe("sha256:aaaaaaaaaaaa");
  expect(shortVersion("1.2.3")).toBe("1.2.3");
  expect(shortChecksum("sha256:" + digest)).toBe("sha256:aaaaaaaaaaaa");
  expect(shortChecksum("md5:abc")).toBe("md5:abc");
});

it("orders tags: latest, semver descending, others by recency, sha-* last", () => {
  const tags = [
    "sha-abc",
    "edge",
    "1.2.0",
    "latest",
    "1.10.0",
    "main",
    "2.0.0-rc.1",
    "2.0.0",
  ].map((name, i) => ({ name, version_id: "v" + i }));
  const created = new Map([
    ["v1", "2024-01-01T00:00:00Z"], // edge
    ["v5", "2024-03-01T00:00:00Z"], // main (newer)
  ]);
  expect(orderTags(tags, created).map((t) => t.name)).toEqual([
    "latest",
    "2.0.0",
    "2.0.0-rc.1",
    "1.10.0",
    "1.2.0",
    "main",
    "edge",
    "sha-abc",
  ]);
});

it("names packages without the library/ prefix and drops a repeated namespace", () => {
  expect(
    packageName({
      id: "1",
      coordinate: "library/alpine",
      display_name: "alpine",
    }),
  ).toBe("alpine");
  expect(
    extraNamespace({ id: "1", coordinate: "team/app", namespace: "team" }),
  ).toBe("");
  expect(
    extraNamespace({ id: "1", coordinate: "app", namespace: "team" }),
  ).toBe("team");
});

it("renders real install commands per format", () => {
  expect(pullCommand("oci", "dockerhub", "alpine", "latest")).toMatch(
    /^docker pull .*\/dockerhub\/alpine:latest$/,
  );
  expect(pullCommand("oci", "dockerhub", "alpine", digest)).toContain(
    "@sha256:" + digest,
  );
  expect(pullCommand("npm", "web", "left-pad", "1.0.0")).toContain(
    "npm install left-pad@1.0.0 --registry",
  );
  expect(pullCommand("pypi", "py", "requests", "2.0")).toContain(
    "pip install requests==2.0 --index-url",
  );
});

function setup(extra: (url: string) => Response | undefined) {
  const api = mockAPI((url) => extra(url));
  vi.stubGlobal("fetch", api.fetcher);
}

it("lists latest version, count and updated time and shows Docker Hub names plainly", async () => {
  setup((url) =>
    url.startsWith("/api/v1/packages")
      ? json({
          items: [
            {
              id: "p1",
              coordinate: "library/alpine",
              display_name: "alpine",
              namespace: "library",
              feed_id: ids.feed,
              feed_slug: "dockerhub",
              format: "oci",
              source_id: ids.source,
              origin: "remote",
              metadata: {},
              latest_version: "latest",
              version_count: 3,
              tag_count: 2,
              updated_at: "2024-05-01T00:00:00Z",
            },
          ],
          page,
          total: 1,
        })
      : undefined,
  );
  render(<App />);
  await signInWithToken();
  nav("Packages");
  await screen.findByText("alpine");
  expect(screen.queryByText("library/alpine")).toBeNull();
  expect(screen.queryByText("library")).toBeNull();
  expect(screen.getByText("latest")).toBeTruthy();
  expect(screen.getByText("3")).toBeTruthy();
});

it("shows the package drawer with an install command, short digests and ordered tags", async () => {
  setup((url) => {
    const path = url.split("?")[0];
    if (path.startsWith("/api/v1/packages"))
      return json({
        items: [
          {
            id: "p1",
            coordinate: "library/alpine",
            display_name: "alpine",
            namespace: "library",
            feed_id: ids.feed,
            feed_slug: "dockerhub",
            format: "oci",
            source_id: ids.source,
            origin: "remote",
            metadata: {},
            latest_version: "latest",
            version_count: 1,
            tag_count: 2,
          },
        ],
        page,
        total: 1,
      });
    if (path.endsWith("/versions"))
      return json({
        items: [
          {
            id: "v1",
            component_id: "p1",
            version: index,
            revision: 1,
            created_at: "2024-05-01T00:00:00Z",
          },
          {
            id: "v3",
            component_id: "p1",
            version: "e".repeat(64),
            revision: 1,
            created_at: "2024-05-01T00:00:00Z",
            pulled_by_digest: true,
          },
          ...(url.includes("top_level")
            ? []
            : [
                {
                  id: "v2",
                  component_id: "p1",
                  version: digest,
                  revision: 1,
                  created_at: "2024-05-01T00:00:00Z",
                  parent_version: index,
                  platform: "linux/arm64/v8",
                },
              ]),
        ],
        page,
      });
    if (path.endsWith("/tags"))
      return json({
        items: [
          { name: "sha-1234", version_id: "v1", revision: 1 },
          {
            name: "latest",
            version_id: "v1",
            revision: 1,
            observed_at: "2024-05-02T00:00:00Z",
          },
        ],
        page,
      });
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Packages");
  fireEvent.click(await screen.findByText("alpine"));
  const drawer = await screen.findByRole("dialog", { name: "alpine" });
  expect(
    within(drawer).getByText(/docker pull .*\/production\/alpine:latest/),
  ).toBeTruthy();
  await within(drawer).findByText("sha256:" + "c".repeat(12));
  // The platform manifest is grouped under the index, never a row of its own.
  expect(within(drawer).getByText("linux/arm64/v8")).toBeTruthy();
  // A tag-less top-level manifest is shown as pulled by digest.
  expect(within(drawer).getByText("pulled by digest")).toBeTruthy();
  expect(within(drawer).queryByText("sha256:aaaaaaaaaaaa")).toBeNull();
  // The full digest is available on hover.
  expect(drawer.querySelector(`[title="sha256:${index}"]`)).toBeTruthy();
  fireEvent.click(within(drawer).getByRole("tab", { name: "Tags" }));
  const rows = await within(drawer).findAllByRole("row");
  expect(rows[1].textContent).toContain("latest");
  expect(rows[2].textContent).toContain("sha-1234");
});

it("explains why a local account cannot use federated search", async () => {
  setup((url) => {
    const path = url.split("?")[0];
    if (path === "/api/v1/federation/status")
      return json({
        instance_id: "i",
        state: "active",
        health: "ok",
        alerts: [],
        protocol: { min: 1, max: 1 },
      });
    if (path === "/api/v1/federation/search")
      return json(
        {
          code: "not_federated",
          message: "only federated principals can be asserted to other members",
          request_id: "r".repeat(16),
        },
        403,
      );
    return undefined;
  });
  render(<App />);
  await signInWithToken();
  nav("Packages");
  fireEvent.click(await screen.findByRole("radio", { name: "Federation" }));
  await screen.findByText("Federated search needs a federated sign-in");
});
