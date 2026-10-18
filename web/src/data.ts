import { useMemo } from "react";
import type { Client } from "./api";
import type { Feed, Source } from "./generated";
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
