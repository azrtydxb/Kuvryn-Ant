import { brandIcons } from "./brandIcons";
import type { Source } from "./generated";
import { Icon } from "./ds/bundle";
import {
  defaultIcons,
  iconForSource,
  presetsFor,
  type IconSpec,
  type SourceLike,
  type Upstream,
} from "./upstreams";

export type IconSize = "sm" | "md" | "lg";
const glyphPx: Record<IconSize, number> = { sm: 13, md: 17, lg: 24 };

/** A logo or glyph on a neutral tile; decorative unless `label` is given. */
export function Mark({
  icon,
  size = "md",
  label,
  className = "",
}: {
  icon: IconSpec;
  size?: IconSize;
  label?: string;
  className?: string;
}) {
  const a11y = label
    ? ({ role: "img", "aria-label": label } as const)
    : ({ "aria-hidden": true } as const);
  if ("brand" in icon) {
    const b = brandIcons[icon.brand];
    return (
      <span
        className={`uicon uicon--${size} ${className}`.trim()}
        data-brand={icon.brand}
        style={{ "--ic-d": b.dark, "--ic-l": b.light } as React.CSSProperties}
        {...a11y}
      >
        <svg viewBox="0 0 24 24" focusable="false">
          <path d={b.path} />
        </svg>
      </span>
    );
  }
  return (
    <span
      className={`uicon uicon--glyph uicon--${size} ${icon.tone ? `uicon--${icon.tone}` : ""} ${className}`.trim()}
      data-glyph={icon.glyph}
      {...a11y}
    >
      <Icon name={icon.glyph} size={glyphPx[size]} />
    </span>
  );
}

/**
 * The mark for a source: the upstream's logo for a known remote origin, a
 * globe for a custom remote, a box for local and a sync glyph for replicas.
 */
export function SourceIcon({
  source,
  size = "md",
  label,
  className,
}: {
  source: SourceLike;
  size?: IconSize;
  label?: string;
  className?: string;
}) {
  const { icon } = iconForSource(source);
  return <Mark icon={icon} size={size} label={label} className={className} />;
}

/** Marks of a feed's remote sources: at most `max`, then +N. */
export function FeedSourceIcons({
  sources,
  max = 3,
  cache = false,
}: {
  sources: readonly Pick<Source, "kind" | "format" | "origin" | "name">[];
  max?: number;
  cache?: boolean;
}) {
  const remotes = sources.filter((s) => s.kind === "remote");
  if (!remotes.length && !cache) return null;
  const shown = remotes.slice(0, max);
  const more = remotes.length - shown.length;
  const names = remotes.map((s) => iconForSource(s).upstream?.name ?? s.name);
  return (
    <span
      className="uicons"
      role="img"
      aria-label={
        (cache ? "Build cache. " : "") +
        (remotes.length ? `Upstreams: ${names.join(", ")}` : "")
      }
    >
      {cache && <Mark icon={defaultIcons.cache} size="sm" />}
      {shown.map((s, i) => (
        <SourceIcon key={i} source={s} size="sm" />
      ))}
      {more > 0 && <span className="uicons__more">+{more}</span>}
    </span>
  );
}

export const CUSTOM = "custom";

/** Preset tiles for one format plus a Custom upstream tile. */
export function UpstreamPicker({
  format,
  selected,
  onPick,
}: {
  format: Source["format"];
  selected: string;
  onPick: (upstream: Upstream | undefined) => void;
}) {
  const presets = presetsFor(format);
  return (
    <div
      className="upgrid"
      role="group"
      aria-label={`Upstream presets for ${format}`}
    >
      {presets.map((u) => (
        <button
          key={u.id}
          type="button"
          className="uptile"
          aria-pressed={selected === u.id}
          onClick={() => onPick(u)}
        >
          <Mark icon={u.icon} size="lg" />
          <span className="uptile__name">{u.name}</span>
        </button>
      ))}
      <button
        type="button"
        className="uptile"
        aria-pressed={selected === CUSTOM}
        onClick={() => onPick(undefined)}
      >
        <Mark icon={defaultIcons.remote} size="lg" />
        <span className="uptile__name">Custom upstream</span>
      </button>
    </div>
  );
}
