import { requireETag, type Client } from "./api";
import { Alert, Input, Textarea } from "./ds/bundle";
import type { Source } from "./generated";
import { initial } from "./RemoteSource";
import { CUSTOM, UpstreamPicker } from "./SourceIcon";
import { presetDraft, type Upstream, upstreams } from "./upstreams";
import { val } from "./ui";

export type UpstreamDraft = {
  /** Catalog id of the picked preset, CUSTOM, or "" before a choice. */
  preset: string;
  name: string;
  origin: string;
  redirects: string;
};
export const emptyDraft: UpstreamDraft = {
  preset: "",
  name: "",
  origin: "",
  redirects: "",
};

/** Picking a preset prefills name, origin and redirect origins; every field stays editable. */
export function pickPreset(
  current: UpstreamDraft,
  upstream: Upstream | undefined,
): UpstreamDraft {
  if (upstream) return { preset: upstream.id, ...presetDraft(upstream) };
  // Custom keeps what the user typed, but drops values a preset supplied.
  const fromPreset = upstreams.some((u) => u.id === current.preset);
  return fromPreset
    ? { ...emptyDraft, preset: CUSTOM }
    : { ...current, preset: CUSTOM };
}

export function parseRedirects(text: string): string[] {
  return text
    .split(/\r?\n/)
    .map((s) => s.trim())
    .filter(Boolean);
}

/** The first reason the draft cannot be saved, if any. */
export function draftProblem(d: UpstreamDraft): string | undefined {
  const origin = d.origin.trim();
  if (!origin) return "Enter the upstream HTTPS base URL.";
  if (!/^https:\/\/.+/i.test(origin))
    return "The upstream URL must start with https://.";
  if (parseRedirects(d.redirects).length > 16)
    return "At most 16 redirect origins are supported.";
  return undefined;
}

export function UpstreamFields({
  format,
  draft,
  setDraft,
  nameHint,
}: {
  format: Source["format"];
  draft: UpstreamDraft;
  setDraft: (d: UpstreamDraft) => void;
  nameHint?: string;
}) {
  const note = upstreams.find((u) => u.id === draft.preset)?.preset?.note;
  return (
    <>
      <UpstreamPicker
        format={format}
        selected={draft.preset}
        onPick={(u) => setDraft(pickPreset(draft, u))}
      />
      {note && (
        <Alert tone="info" title="About this upstream">
          {note}
        </Alert>
      )}
      <Input
        label="Source name"
        mono
        required
        maxLength={200}
        value={draft.name}
        onChange={val((name) => setDraft({ ...draft, name }))}
        hint={nameHint ?? "Up to 200 bytes."}
      />
      <Input
        label="Upstream HTTPS base URL"
        mono
        required
        type="url"
        pattern="https://.*"
        maxLength={4096}
        value={draft.origin}
        onChange={val((origin) => setDraft({ ...draft, origin }))}
        hint="No credentials, query, fragment, or dot path segments."
      />
      <Textarea
        label="Allowed redirect origins"
        mono
        rows={3}
        value={draft.redirects}
        onChange={val((redirects) => setDraft({ ...draft, redirects }))}
        hint="One HTTPS origin per line, at most 16. Empty denies cross-origin redirects."
      />
      <p className="muted small">
        Credentials are optional. Add them on the source's Upstream tab after it
        is created; they are stored encrypted and never shown again.
      </p>
    </>
  );
}

/**
 * Creates a remote source and stores its upstream configuration. When the
 * source exists but the configuration could not be saved, `configError` is
 * set so the caller can open the source and finish there.
 */
export async function createRemoteSource(
  client: Client,
  format: Source["format"],
  draft: UpstreamDraft,
): Promise<{ source: Source; configError?: Error }> {
  const made = await client.call("createSource", {
    body: { name: draft.name.trim(), kind: "remote", format },
  });
  try {
    await client.call("setRemoteSourceConfig", {
      params: { id: made.data.id },
      etag: requireETag(made.etag),
      body: {
        ...initial,
        origin: draft.origin.trim(),
        redirect_origins: parseRedirects(draft.redirects),
      },
    });
  } catch (e) {
    return {
      source: made.data,
      configError: e instanceof Error ? e : Error(String(e)),
    };
  }
  return { source: made.data };
}
