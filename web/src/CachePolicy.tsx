import { useState } from "react";
import type { CachePolicy, CachePolicyInput, Feed } from "./generated";
import { Badge, Input } from "./ds/bundle";
import { bytes, val } from "./ui";

export const DAY = 86400;
const GIB = 2 ** 30;

/** Build caches (buildx layer cache, sccache) can only be OCI or raw feeds. */
export const cacheFormats: ReadonlyArray<Feed["format"]> = ["oci", "raw"];

export const isCache = (f: { purpose?: string }) => f.purpose === "cache";

/** Badge shown next to a cache feed's name. */
export function CacheBadge() {
  return (
    <Badge tone="pillar" icon="zap">
      Cache
    </Badge>
  );
}

/** "7 days", "36 hours" or "90 minutes" for a duration in seconds. */
export function idleLabel(seconds: number) {
  if (seconds % DAY === 0) {
    const d = seconds / DAY;
    return `${d} day${d === 1 ? "" : "s"}`;
  }
  if (seconds >= 3600) {
    const h = Math.round(seconds / 3600);
    return `${h} hour${h === 1 ? "" : "s"}`;
  }
  const m = Math.max(1, Math.round(seconds / 60));
  return `${m} minute${m === 1 ? "" : "s"}`;
}

/** One-line summary of a cache policy for the feed header and lists. */
export function policySummary(p?: CachePolicy) {
  if (!p) return "";
  return `Evicted after ${idleLabel(p.max_idle_seconds)} idle${
    p.max_bytes ? ` · limited to ${bytes(p.max_bytes)}` : " · no size limit"
  }`;
}

export type PolicyDraft = { days: string; gib: string };

export function draftFrom(p?: CachePolicy): PolicyDraft {
  return {
    days: String(((p?.max_idle_seconds ?? 7 * DAY) / DAY) as number),
    gib: p?.max_bytes ? String(p.max_bytes / GIB) : "",
  };
}

/** Converts the form values; undefined means the values are invalid. */
export function draftToInput(d: PolicyDraft): CachePolicyInput | undefined {
  const days = Number(d.days);
  const idle = Math.round(days * DAY);
  if (!Number.isFinite(days) || idle < 60) return undefined;
  const gib = d.gib.trim() === "" ? 0 : Number(d.gib);
  if (!Number.isFinite(gib) || gib < 0) return undefined;
  return { max_idle_seconds: idle, max_bytes: Math.round(gib * GIB) };
}

export function useDraft(p?: CachePolicy) {
  return useState<PolicyDraft>(draftFrom(p));
}

/** Max idle and max size inputs of the cache policy. */
export function CachePolicyFields({
  draft,
  set,
}: {
  draft: PolicyDraft;
  set: (d: PolicyDraft) => void;
}) {
  const valid = draftToInput(draft) !== undefined;
  return (
    <div className="cols">
      <Input
        label="Evict when idle for (days)"
        type="number"
        mono
        min={0.001}
        step="any"
        value={draft.days}
        onChange={val((days) => set({ ...draft, days }))}
        error={
          valid
            ? undefined
            : "At least one minute; a size must not be negative."
        }
        hint="Idle is the latest of created, last accessed and last downloaded."
      />
      <Input
        label="Size limit (GiB)"
        type="number"
        mono
        min={0}
        step="any"
        value={draft.gib}
        placeholder="No limit"
        onChange={val((gib) => set({ ...draft, gib }))}
        hint="Least recently used content is evicted until the feed is under it."
      />
    </div>
  );
}
