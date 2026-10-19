import { Copyable, Mono } from "./ui";

// Version and tag presentation shared by the package list and detail views.

const hex64 = /^[0-9a-f]{64}$/;

// OCI versions are manifest digests stored without their algorithm prefix.
export function isDigestVersion(v: string): boolean {
  return hex64.test(v);
}
export function fullDigest(v: string): string {
  return "sha256:" + v;
}
// "sha256:" plus 12 hex digits; anything else is returned unchanged.
export function shortVersion(v: string): string {
  return isDigestVersion(v) ? "sha256:" + v.slice(0, 12) : v;
}

// A version name: digests are shortened with the full value on hover and copy.
export function VersionName({
  version,
  copy,
}: {
  version: string;
  copy?: boolean;
}) {
  if (!isDigestVersion(version)) return <Mono>{version}</Mono>;
  if (copy)
    return (
      <Copyable text={fullDigest(version)} display={shortVersion(version)} />
    );
  return (
    <span className="mono" title={fullDigest(version)}>
      {shortVersion(version)}
    </span>
  );
}

type Core = { nums: number[]; release: boolean };
function parseCore(tag: string): Core | undefined {
  const m = /^[vV]?(\d+(?:\.\d+)*)(.*)$/.exec(tag);
  if (!m) return undefined;
  return { nums: m[1].split(".").map(Number), release: m[2] === "" };
}
function compareCore(a: Core, b: Core): number {
  for (let i = 0; i < Math.max(a.nums.length, b.nums.length); i++) {
    const d = (a.nums[i] ?? 0) - (b.nums[i] ?? 0);
    if (d) return d;
  }
  return a.release === b.release ? 0 : a.release ? 1 : -1;
}

export type TagLike = { name: string; version_id: string };

// Display order for a package's tags: "latest" first, then semver-looking tags
// highest first (releases before prereleases of the same number), then other
// tags newest first by the creation time of the version they point at, with CI
// commit tags (sha-*) last. `created` maps version id -> creation time.
export function orderTags<T extends TagLike>(
  tags: T[],
  created: ReadonlyMap<string, string> = new Map(),
): T[] {
  const rank = (t: T) =>
    t.name === "latest"
      ? 0
      : parseCore(t.name)
        ? 1
        : t.name.startsWith("sha-")
          ? 3
          : 2;
  const time = (t: T) => Date.parse(created.get(t.version_id) ?? "") || 0;
  return [...tags].sort((a, b) => {
    const ra = rank(a);
    const rb = rank(b);
    if (ra !== rb) return ra - rb;
    if (ra === 1) {
      const d = compareCore(parseCore(b.name)!, parseCore(a.name)!);
      if (d) return d;
    }
    return time(b) - time(a) || a.name.localeCompare(b.name);
  });
}

// "sha256:" plus 12 hex digits of a full checksum; other text is unchanged.
export function shortChecksum(d: string): string {
  return /^sha256:[0-9a-f]{64}$/.test(d) ? d.slice(0, 19) : d;
}
