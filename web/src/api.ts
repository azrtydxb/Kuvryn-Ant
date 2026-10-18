import { routes, type Operations } from "./generated";
export class APIError extends Error {
  constructor(
    public status: number,
    public code: string,
    public requestId: string,
  ) {
    super(
      status === 401
        ? "Authentication rejected or expired. Sign out and sign in again."
        : status === 403
          ? "Permission denied. This operation requires an appropriate resource grant."
          : status === 412
            ? "Configuration changed. Reload this view before saving again."
            : status === 409
              ? "Configuration conflicts with existing data. Check names, source priorities, and policy targets."
              : status === 400
                ? "The server rejected this configuration. Check field values and policy rules."
                : status === 429
                  ? "Too many requests. Wait before retrying."
                  : status >= 500
                    ? "Service unavailable. Check the service and try again."
                    : `Request failed (HTTP ${status}).`,
    );
  }
}
// One client per in-memory session. Disposal aborts reads and writes and forgets its token.
export class Client {
  private controller = new AbortController();
  private pending = new Map<string, string>();
  constructor(
    private token = "",
    private csrf = "",
  ) {}
  static browser(csrf: string) {
    return new Client("", csrf);
  }
  dispose() {
    this.token = "";
    this.csrf = "";
    this.pending.clear();
    this.controller.abort();
  }
  async call<K extends keyof Operations>(
    operation: K,
    options: {
      params?: Record<string, string>;
      query?: Record<string, string>;
      body?: Operations[K]["body"];
      etag?: string;
    } = {},
  ): Promise<{ data: Operations[K]["response"]; etag: string | null }> {
    const route = routes[operation];
    let path: string = route.path;
    for (const [key, value] of Object.entries(options.params ?? {}))
      path = path.replace(`{${key}}`, encodeURIComponent(value));
    if (path.includes("{")) throw Error("Missing resource identifier.");
    return this.send(
      route.method,
      path,
      route.idempotent,
      options.query,
      options.body,
      options.etag,
    );
  }
  // Endpoints that are not in the generated contract yet (see gaps.ts). Same
  // session, abort, CSRF and idempotency behavior as call(); mutations are
  // always idempotency-keyed. Only /api/v1 paths are reachable.
  async extra<T = unknown>(
    method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE",
    path: string,
    options: {
      query?: Record<string, string>;
      body?: unknown;
      etag?: string;
    } = {},
  ): Promise<{ data: T; etag: string | null }> {
    if (!path.startsWith("/api/v1/") || path.includes("{"))
      throw Error("Invalid resource path.");
    return this.send(
      method,
      path,
      method !== "GET",
      options.query,
      options.body,
      options.etag,
      "empty",
    );
  }
  // Plain-text export (audit CSV / NDJSON). Same session and CSRF handling.
  async exportText(
    path: string,
    query: Record<string, string>,
  ): Promise<string> {
    if (!path.startsWith("/api/v1/") || path.includes("{"))
      throw Error("Invalid resource path.");
    const { data } = await this.send(
      "GET",
      path,
      false,
      query,
      undefined,
      undefined,
      "text",
    );
    return data as string;
  }
  private async send(
    method: string,
    path: string,
    idempotent: boolean,
    queryInput?: Record<string, string>,
    requestBody?: unknown,
    etag?: string,
    mode: "json" | "empty" | "text" = "json",
  ): Promise<{ data: any; etag: string | null }> {
    if (this.token && !/^[\x21-\x7e]+$/.test(this.token)) {
      throw Error(
        "Token contains invalid characters. Sign out and enter it again.",
      );
    }
    const query = new URLSearchParams(queryInput);
    const url = path + (query.size ? "?" + query : "");
    const body =
      requestBody === undefined ? undefined : JSON.stringify(requestBody);
    const headers = new Headers({ Accept: "application/json" });
    if (this.token) headers.set("Authorization", "Bearer " + this.token);
    if (body !== undefined) headers.set("Content-Type", "application/json");
    if (etag) headers.set("If-Match", etag);
    // Retain only a digest for replay matching, never submitted credentials.
    const fingerprint = idempotent
      ? Array.from(
          new Uint8Array(
            await crypto.subtle.digest(
              "SHA-256",
              new TextEncoder().encode(
                JSON.stringify([method, path, body, etag]),
              ),
            ),
          ),
          (b) => b.toString(16).padStart(2, "0"),
        ).join("")
      : "";
    if (this.csrf && method !== "GET") headers.set("X-CSRF-Token", this.csrf);
    if (idempotent) {
      if (!this.pending.has(fingerprint))
        this.pending.set(fingerprint, crypto.randomUUID());
      headers.set("Idempotency-Key", this.pending.get(fingerprint)!);
    }
    let response: Response;
    try {
      response = await fetch(url, {
        method,
        headers,
        body,
        signal: this.controller.signal,
        cache: "no-store",
        credentials: this.csrf ? "same-origin" : "omit",
        redirect: "error",
      });
    } catch {
      throw Error(
        this.controller.signal.aborted
          ? "Session ended."
          : idempotent
            ? "Cannot reach the service. Your change may have completed; retry unchanged to reuse its idempotency key."
            : "Cannot reach the service. The operation may have completed. Token issuance is not replayable; do not assume a retry returns the same token.",
      );
    }
    if (!response.ok && response.status < 500) this.pending.delete(fingerprint);
    if (!response.ok) {
      const error = await response.json().catch(() => ({}));
      // Do not display arbitrary server messages: they can contain submitted secrets.
      throw new APIError(
        response.status,
        typeof error.code === "string" ? error.code : "",
        response.headers.get("X-Request-ID") ?? "",
      );
    }
    let data: unknown;
    try {
      data =
        mode === "text"
          ? await response.text()
          : mode === "empty" && response.status === 204
            ? null
            : await response.json();
    } catch {
      throw Error(
        idempotent
          ? "The service returned an unreadable response. Retry unchanged to check the same operation."
          : "The service returned an unreadable response. The operation may have completed; token issuance cannot be replayed.",
      );
    }
    this.pending.delete(fingerprint);
    return { data, etag: response.headers.get("ETag") };
  }
}
export function requireETag(etag: string | null): string {
  if (!etag || !/^"[1-9][0-9]*"$/.test(etag))
    throw Error("No valid revision was returned. Reload before editing.");
  return etag;
}
