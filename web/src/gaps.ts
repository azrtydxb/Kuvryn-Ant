// Optional-feature helpers. Most management endpoints are always present, but
// a server older than this console may not have them yet. HTTP 404, 405 or 501
// means "not on this server yet" and renders the Unavailable state instead of
// an error, so the console degrades instead of breaking.
import { APIError, type Client } from "./api";
import { useResource } from "./ui";
import type { Operations } from "./generated";

export function isUnavailable(error: unknown): boolean {
  return (
    error instanceof APIError &&
    (error.status === 404 || error.status === 405 || error.status === 501)
  );
}

export type Optional<T> =
  | { state: "loading" }
  | { state: "unavailable" }
  | { state: "error"; error: unknown }
  | { state: "ready"; data: T };

// Reads a GET endpoint that may not exist yet. `enabled` false keeps it idle.
// Reads a generated operation that this server may not provide.
export function useOptionalCall<K extends keyof Operations>(
  client: Client,
  operation: K,
  options: {
    params?: Record<string, string>;
    query?: Record<string, string>;
  } = {},
) {
  const key = JSON.stringify(options);
  const r = useResource(
    () => client.call(operation, options),
    [client, operation, key],
  );
  const state: Optional<Operations[K]["response"]> = r.loading
    ? { state: "loading" }
    : r.error
      ? isUnavailable(r.error)
        ? { state: "unavailable" }
        : { state: "error", error: r.error }
      : r.value
        ? { state: "ready", data: r.value.data }
        : { state: "unavailable" };
  return { ...state, reload: r.reload } as Optional<
    Operations[K]["response"]
  > & {
    reload: () => void;
  };
}
