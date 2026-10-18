import { useMemo } from "react";
import { useOptionalCall, type Optional } from "./gaps";
import type { Client } from "./api";
import type {
  Feed,
  FeedStats,
  Source,
  SourceHealth,
  SourceStats,
} from "./generated";
import { useResource } from "./ui";

const MAX_PAGES = 10;
// Walks every cursor page (200 per page, at most 2,000 records) so pickers and
// counts reflect what this principal can see, not just the first page.
export async function loadAllSources(client: Client): Promise<Source[]> {
  const all: Source[] = [];
  let cursor = "";
  for (let i = 0; i < MAX_PAGES; i++) {
    const { data } = await client.call("listSources", {
      query: { limit: "200", ...(cursor ? { cursor } : {}) },
    });
    all.push(...data.items);
    if (!data.page.has_more || !data.page.next_cursor) break;
    cursor = data.page.next_cursor;
  }
  return all;
}
export async function loadAllFeeds(client: Client): Promise<Feed[]> {
  const all: Feed[] = [];
  let cursor = "";
  for (let i = 0; i < MAX_PAGES; i++) {
    const { data } = await client.call("listFeeds", {
      query: { limit: "200", ...(cursor ? { cursor } : {}) },
    });
    all.push(...data.items);
    if (!data.page.has_more || !data.page.next_cursor) break;
    cursor = data.page.next_cursor;
  }
  return all;
}
export function useAllSources(client: Client) {
  const r = useResource(
    async () => ({ data: await loadAllSources(client), etag: null }),
    [client],
  );
  return { ...r, items: useMemo(() => r.value?.data ?? [], [r.value]) };
}
export function useAllFeeds(client: Client) {
  const r = useResource(
    async () => ({ data: await loadAllFeeds(client), etag: null }),
    [client],
  );
  return { ...r, items: useMemo(() => r.value?.data ?? [], [r.value]) };
}
// Source pickers show only sources of the feed's format, never raw UUIDs.
export function sourcesForFormat(sources: Source[], format: string) {
  return sources.filter((s) => s.format === format);
}
export function sourceLabel(s: Source) {
  return `${s.name} (${s.kind})`;
}

function rows<T>(r: Optional<{ items: T[] }>) {
  return r.state === "ready" ? r.data.items : [];
}
// Per-feed and per-source counts and source health, when the server has them.
export function useOpsMaps(client: Client) {
  const feeds = useOptionalCall(client, "listFeedStats", {
    query: { limit: "200" },
  });
  const sources = useOptionalCall(client, "listSourceStats", {
    query: { limit: "200" },
  });
  const health = useOptionalCall(client, "listSourceHealth", {
    query: { limit: "200" },
  });
  return {
    feeds: new Map<string, FeedStats>(rows(feeds).map((x) => [x.feed_id, x])),
    sources: new Map<string, SourceStats>(
      rows(sources).map((x) => [x.source_id, x]),
    ),
    health: new Map<string, SourceHealth>(
      rows(health).map((x) => [x.source_id, x]),
    ),
    reload: () => {
      feeds.reload();
      sources.reload();
      health.reload();
    },
  };
}
