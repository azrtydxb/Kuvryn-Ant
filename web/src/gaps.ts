// Console features whose backend endpoints are not in api/openapi.yaml yet
// ("Backend requirements" items 1, 3-21). Everything the console assumes about
// those endpoints lives here so wiring a real endpoint later means moving one
// definition into the generated contract. Shapes are assumptions taken from the
// design brief, so every field is optional and every reader is defensive.
//
// A feature is "available" only when the server answers; HTTP 404, 405 or 501
// means "not on this server yet" and renders the Unavailable state instead of an
// error.
import { APIError, type Client } from "./api";
import { useResource } from "./ui";
import type { DiscoveredPackage, PageInfo, PublishRule } from "./generated";

export const paths = {
  packageSearch: "/api/v1/packages",
  reorder: (feed: string) =>
    `/api/v1/feeds/${encodeURIComponent(feed)}/sources`,
  resolve: (feed: string) =>
    `/api/v1/feeds/${encodeURIComponent(feed)}/resolve`,
  policyTest: (feed: string) =>
    `/api/v1/feeds/${encodeURIComponent(feed)}/publish-policy:test`,
  feed: (feed: string) => `/api/v1/feeds/${encodeURIComponent(feed)}`,
  tokens: "/api/v1/tokens",
  bindings: "/api/v1/identity-bindings",
  audit: "/api/v1/audit",
  auditSinks: "/api/v1/audit/sinks",
  traffic: "/api/v1/metrics/traffic",
  storage: "/api/v1/storage",
  settings: "/api/v1/settings",
  retention: "/api/v1/retention",
  sourceTest: (id: string) =>
    `/api/v1/sources/${encodeURIComponent(id)}/connection-test`,
  sourcePurge: (id: string) =>
    `/api/v1/sources/${encodeURIComponent(id)}/cache-purge`,
  redeliver: (hook: string, event: number) =>
    `/api/v1/webhooks/${encodeURIComponent(hook)}/deliveries/${event}/redeliver`,
  effective: (principal: string) =>
    `/api/v1/principals/${encodeURIComponent(principal)}/effective-permissions`,
};

export type PackageSearchItem = Partial<DiscoveredPackage> & {
  id: string;
  coordinate: string;
  feed_id?: string;
  feed_slug?: string;
  format?: string;
  origin?: "local" | "cached";
  latest_version?: string;
  version_count?: number;
  updated_at?: string;
  size_bytes?: number;
};
export type PackageSearchPage = {
  items: PackageSearchItem[];
  page?: PageInfo;
  total?: number;
};
export type Resolution = {
  source?: { id?: string; name?: string; kind?: string };
  shadowed?: { id?: string; name?: string }[];
  cache?: { fresh?: boolean; ttl_remaining_seconds?: number };
};
export type PolicyTestResult = {
  allowed?: boolean;
  rule_id?: string;
  target_source_id?: string;
  reason?: string;
};
export type PolicyTestBody = {
  default_source_id?: string;
  rules: PublishRule[];
  coordinate: string;
  principal_id?: string;
};
export type TokenRecord = {
  id: string;
  principal_id?: string;
  scopes?: { feed_id?: string; namespace?: string; actions: string[] }[];
  created_at?: string;
  expires_at?: string;
  last_used_at?: string;
  revoked?: boolean;
};
export type BindingRecord = {
  issuer: string;
  subject: string;
  principal_id: string;
};
export type AuditRecord = {
  id?: string;
  at?: string;
  principal_id?: string;
  principal_subject?: string;
  principal_kind?: string;
  action?: string;
  resource?: string;
  result?: string;
};
export type Traffic = {
  labels?: string[];
  pulls?: number[];
  publishes?: number[];
  totals?: { pulls?: number; publishes?: number };
  cache_hit_ratio?: number;
};
export type Storage = {
  backend?: string;
  endpoint?: string;
  bucket?: string;
  prefix?: string;
  path_style?: boolean;
  staging_path?: string;
  bucket_used_bytes?: number;
  bucket_capacity_bytes?: number;
  staging_used_bytes?: number;
  staging_capacity_bytes?: number;
  local_bytes?: number;
  cached_bytes?: number;
  dedup_saved_bytes?: number;
};
export type InstanceSettings = {
  display_name?: string;
  public_url?: string;
  default_anonymous_read?: boolean;
  session_max_age_hours?: number;
  idle_timeout_minutes?: number;
  read_only?: string[];
};
export type Retention = {
  evict_cached_after_days?: number | null;
  gc_unreferenced_blobs?: boolean;
  gc_schedule?: string;
};
export type EffectivePermissions = {
  items?: {
    kind?: string;
    feed_id?: string;
    source_id?: string;
    namespace?: string;
    effect?: string;
    actions?: string[];
  }[];
};
// Optional additions to existing records (items 10-12, 17).
export type FeedStats = {
  package_count?: number;
  size_bytes?: number;
  pull_count?: number;
  updated_at?: string;
};
export type SourceStats = {
  size_bytes?: number;
  feed_count?: number;
  origin?: string;
  health?: {
    state?: "healthy" | "degraded" | "failing";
    message?: string;
    status_code?: number;
    p95_ms?: number;
    last_success_at?: string;
    last_error_at?: string;
  };
};
export type WebhookStats = {
  success_rate_7d?: number;
  last_delivery_at?: string;
  last_status?: number;
};
export type Readiness = {
  readiness?: {
    database?: { ready?: boolean; latency_ms?: number };
    storage?: { ready?: boolean; backend?: string };
    writer_lease?: { held?: boolean; holder?: string };
  };
};

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
export function useOptional<T>(
  client: Client,
  path: string,
  query: Record<string, string> = {},
  enabled = true,
) {
  const key = JSON.stringify(query);
  const r = useResource<T | undefined>(
    async () =>
      enabled
        ? client.extra<T>("GET", path, { query })
        : { data: undefined, etag: null },
    [client, path, key, enabled],
  );
  const state: Optional<T> = !enabled
    ? { state: "loading" }
    : r.loading
      ? { state: "loading" }
      : r.error
        ? isUnavailable(r.error)
          ? { state: "unavailable" }
          : { state: "error", error: r.error }
        : r.value && r.value.data !== undefined
          ? { state: "ready", data: r.value.data }
          : { state: "unavailable" };
  return { ...state, reload: r.reload, loading: r.loading } as Optional<T> & {
    reload: () => void;
    loading: boolean;
  };
}
