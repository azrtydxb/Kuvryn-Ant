import { afterEach, describe, expect, it, vi } from "vitest";
import { APIError, Client, requireETag } from "./api";
afterEach(() => vi.unstubAllGlobals());
const ok = (data: unknown = {}) =>
  new Response(JSON.stringify(data), {
    headers: { ETag: '"9"', "Content-Type": "application/json" },
  });
describe("management transport", () => {
  it("uses same-origin paths, bearer headers, revision guards and no browser cookies", async () => {
    const fetcher = vi.fn().mockResolvedValue(ok());
    vi.stubGlobal("fetch", fetcher);
    await new Client("secret").call("updateFeed", {
      params: { id: "feed" },
      etag: '"8"',
      body: { slug: "updated" },
    });
    const [url, request] = fetcher.mock.calls[0];
    expect(url).toBe("/api/v1/feeds/feed");
    expect(url).not.toContain("secret");
    expect(request.headers.get("Authorization")).toBe("Bearer secret");
    expect(request.headers.get("If-Match")).toBe('"8"');
    expect(request.headers.get("Idempotency-Key")).toBeTruthy();
    expect(request.credentials).toBe("omit");
    expect(request.redirect).toBe("error");
    expect(request.cache).toBe("no-store");
  });
  it("reuses idempotency keys after network, server, and truncated successful responses", async () => {
    const fetcher = vi
      .fn()
      .mockRejectedValueOnce(Error("network"))
      .mockResolvedValueOnce(new Response("{}", { status: 503 }))
      .mockResolvedValueOnce(new Response("{"))
      .mockResolvedValueOnce(ok())
      .mockResolvedValueOnce(ok());
    vi.stubGlobal("fetch", fetcher);
    const client = new Client("secret");
    const body = { slug: "one", format: "oci" as const };
    for (let i = 0; i < 3; i++)
      await expect(client.call("createFeed", { body })).rejects.toThrow();
    await client.call("createFeed", { body });
    await client.call("createFeed", { body });
    const keys = fetcher.mock.calls.map((c) =>
      c[1].headers.get("Idempotency-Key"),
    );
    expect(new Set(keys.slice(0, 4)).size).toBe(1);
    expect(keys[4]).not.toBe(keys[0]);
  });
  it("does not repeat an old mutation key for changed payloads", async () => {
    const fetcher = vi.fn().mockRejectedValue(Error("network"));
    vi.stubGlobal("fetch", fetcher);
    const c = new Client();
    await expect(
      c.call("createFeed", { body: { slug: "one", format: "oci" } }),
    ).rejects.toThrow();
    await expect(
      c.call("createFeed", { body: { slug: "two", format: "oci" } }),
    ).rejects.toThrow();
    expect(fetcher.mock.calls[0][1].headers.get("Idempotency-Key")).not.toBe(
      fetcher.mock.calls[1][1].headers.get("Idempotency-Key"),
    );
  });
  it("never includes server-supplied messages in displayed errors", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValue(
          new Response(
            JSON.stringify({ message: "secret-token", code: "forbidden" }),
            { status: 403, headers: { "X-Request-ID": "safe-request-id" } },
          ),
        ),
    );
    try {
      await new Client().call("listFeeds");
      throw Error("expected error");
    } catch (e) {
      expect(e).toBeInstanceOf(APIError);
      expect((e as Error).message).not.toContain("secret-token");
      expect((e as APIError).requestId).toBe("safe-request-id");
    }
  });
  it("aborts in-flight requests when session ends", async () => {
    let signal: AbortSignal | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn((_url, init) => {
        signal = init.signal;
        return new Promise((_resolve, reject) =>
          signal?.addEventListener("abort", () => reject(Error("aborted"))),
        );
      }),
    );
    const c = new Client("secret");
    const request = c.call("listFeeds");
    c.dispose();
    expect(signal?.aborted).toBe(true);
    await expect(request).rejects.toThrow("Session ended");
  });
  it("rejects missing, weak or malformed revisions", () => {
    for (const v of [null, 'W/"2"', "2", '"0"'])
      expect(() => requireETag(v)).toThrow();
    expect(requireETag('"2"')).toBe('"2"');
  });
});
it("rejects invalid token header characters without echoing the secret", async () => {
  const fetcher = vi.fn();
  vi.stubGlobal("fetch", fetcher);
  await expect(
    new Client("private-secret\ninvalid").call("listFeeds"),
  ).rejects.toThrow("Token contains invalid characters");
  expect(fetcher).not.toHaveBeenCalled();
});
