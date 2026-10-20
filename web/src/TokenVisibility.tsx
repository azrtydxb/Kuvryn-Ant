import { useState, type ReactNode } from "react";
import type { TokenCounts } from "./generated";
import { Badge, Switch } from "./ds/bundle";
import { chk } from "./ui";

export const TOKEN_WARN_DAYS = 7;
const DAY = 86400000;
const STORAGE_KEY = "ant.tokens.showInactive";

export type TokenState = "active" | "expired" | "revoked";

/** Lifecycle state of a token. The server computes it; older responses are derived locally. */
export function tokenState(t: {
  state?: TokenState;
  status?: TokenState;
  revoked_at?: string;
  expires_at: string;
}): TokenState {
  return (
    t.state ??
    t.status ??
    (t.revoked_at
      ? "revoked"
      : Date.parse(t.expires_at) <= Date.now()
        ? "expired"
        : "active")
  );
}

/** Whole days left, rounded up; null when the token is not near expiry. */
export function expiresSoon(t: {
  state?: TokenState;
  status?: TokenState;
  revoked_at?: string;
  expires_at: string;
}): number | null {
  if (tokenState(t) !== "active") return null;
  const left = Date.parse(t.expires_at) - Date.now();
  if (!(left > 0) || left >= TOKEN_WARN_DAYS * DAY) return null;
  return Math.max(1, Math.ceil(left / DAY));
}

export function expiresText(days: number) {
  return days <= 1 ? "Expires in 1 day" : `Expires in ${days} days`;
}

/** Expired and revoked tokens are hidden unless the viewer opts in; the choice is remembered. */
export function useShowInactiveTokens(): [boolean, (v: boolean) => void] {
  const [show, setShow] = useState(() => {
    try {
      return localStorage.getItem(STORAGE_KEY) === "1";
    } catch {
      return false;
    }
  });
  return [
    show,
    (v) => {
      setShow(v);
      try {
        localStorage.setItem(STORAGE_KEY, v ? "1" : "0");
      } catch {
        /* storage unavailable: the choice lasts for this page only */
      }
    },
  ];
}

export function hiddenText(c: TokenCounts | undefined) {
  if (!c) return "";
  const parts = [
    c.expired > 0 ? `${c.expired} expired` : "",
    c.revoked > 0 ? `${c.revoked} revoked` : "",
  ].filter(Boolean);
  return parts.length ? `${parts.join(" and ")} hidden` : "";
}

export function TokenStateBadge({
  token,
}: {
  token: Parameters<typeof tokenState>[0];
}) {
  const state = tokenState(token);
  const soon = expiresSoon(token);
  if (state === "revoked")
    return (
      <Badge tone="neutral" dot>
        Revoked
      </Badge>
    );
  if (state === "expired")
    return (
      <Badge tone="bad" dot>
        Expired
      </Badge>
    );
  if (soon !== null)
    return (
      <Badge tone="warn" dot>
        {expiresText(soon)}
      </Badge>
    );
  return (
    <Badge tone="good" dot>
      Active
    </Badge>
  );
}

/** Cell content for expired and revoked rows: muted, but still readable. */
export function Dim({
  token,
  children,
}: {
  token: Parameters<typeof tokenState>[0];
  children: ReactNode;
}) {
  return (
    <span className={tokenState(token) === "active" ? undefined : "tok-dead"}>
      {children}
    </span>
  );
}

/** The "show expired and revoked" toggle with the count of what is hidden. */
export function TokenVisibilityBar({
  show,
  onShow,
  counts,
  children,
}: {
  show: boolean;
  onShow: (v: boolean) => void;
  counts?: TokenCounts;
  children?: ReactNode;
}) {
  const hidden = show ? "" : hiddenText(counts);
  return (
    <div className="row tok-bar">
      <label className="row tight">
        <Switch
          aria-label="Show expired and revoked tokens"
          checked={show}
          onChange={chk(onShow)}
        />
        <span className="small">Show expired and revoked</span>
      </label>
      {hidden && (
        <span data-testid="hidden-tokens">
          <Badge tone="neutral">{hidden}</Badge>
        </span>
      )}
      {children}
    </div>
  );
}

/** Date without the time of day, with the full timestamp on hover; keeps token tables narrow. */
export function DateCell({ value }: { value?: string | null }) {
  if (!value) return <>—</>;
  const d = new Date(value);
  if (!Number.isFinite(d.getTime())) return <>{value}</>;
  return <span title={d.toLocaleString()}>{d.toLocaleDateString()}</span>;
}
