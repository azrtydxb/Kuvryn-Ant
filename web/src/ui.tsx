import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useState,
  type ReactNode,
} from "react";
import { APIError } from "./api";
import {
  Alert,
  Badge,
  Button,
  EmptyState,
  Icon,
  IconButton,
  Spinner,
  Toast,
  ToastStack,
} from "./ds/bundle";

type Result<T> = { data: T; etag: string | null };
export function useResource<T>(
  load: () => Promise<Result<T>>,
  dependencies: unknown[],
) {
  const [value, setValue] = useState<Result<T>>();
  const [error, setError] = useState<unknown>();
  const [loading, setLoading] = useState(true);
  const [version, setVersion] = useState(0);
  useEffect(() => {
    let active = true;
    setLoading(true);
    setValue(undefined);
    setError(undefined);
    load()
      .then((v) => {
        if (active) setValue(v);
      })
      .catch((e) => {
        if (active) setError(e);
      })
      .finally(() => {
        if (active) setLoading(false);
      });
    return () => {
      active = false;
    };
  }, [...dependencies, version]);
  return { value, error, loading, reload: () => setVersion((v) => v + 1) };
}

// Server messages are never rendered: they can echo submitted secrets.
export function Failure({
  error,
  className,
}: {
  error: unknown;
  className?: string;
}) {
  if (!error) return null;
  return (
    <Alert tone="bad" className={className}>
      {error instanceof Error
        ? error.message
        : "Unable to complete the request."}
      {error instanceof APIError && error.requestId && (
        <small className="block mono muted">
          Request ID: {error.requestId}
        </small>
      )}
    </Alert>
  );
}

export function useAction() {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const run = useCallback(
    async (fn: () => Promise<unknown>) => {
      if (busy) return false;
      setBusy(true);
      setError(undefined);
      try {
        await fn();
        return true;
      } catch (e) {
        setError(e);
        return false;
      } finally {
        setBusy(false);
      }
    },
    [busy],
  );
  return { busy, error, run, clear: () => setError(undefined) };
}

/* ---------- toasts and backend notes ---------- */
type ToastItem = {
  id: number;
  title: string;
  body?: string;
  tone: "good" | "bad" | "warn" | "info";
};
const ToastContext = createContext<
  (title: string, body?: string, tone?: ToastItem["tone"]) => void
>(() => undefined);
export const useToast = () => useContext(ToastContext);
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const push = useCallback(
    (title: string, body?: string, tone: ToastItem["tone"] = "good") => {
      const id = Date.now() + Math.random();
      setItems((t) => [...t, { id, title, body, tone }]);
      setTimeout(() => setItems((t) => t.filter((x) => x.id !== id)), 4500);
    },
    [],
  );
  return (
    <ToastContext.Provider value={push}>
      {children}
      <ToastStack>
        {items.map((t) => (
          <Toast
            key={t.id}
            tone={t.tone}
            title={t.title}
            onClose={() => setItems((x) => x.filter((y) => y.id !== t.id))}
          >
            {t.body}
          </Toast>
        ))}
      </ToastStack>
    </ToastContext.Provider>
  );
}
export const NotesContext = createContext(false);
// Marks UI that depends on a backend change (design "Backend notes" tweak).
export function Needs({
  children,
  block,
}: {
  children: ReactNode;
  block?: boolean;
}) {
  const on = useContext(NotesContext);
  if (!on) return null;
  const el = (
    <span className="needs" title={"Backend: " + String(children)}>
      <Icon name="server-cog" size={12} />
      API: {children}
    </span>
  );
  return block ? <div className="needs-block">{el}</div> : el;
}

/* ---------- layout ---------- */
export function PageHeading({
  eyebrow,
  title,
  text,
  children,
  back,
}: {
  eyebrow: string;
  title: ReactNode;
  text?: ReactNode;
  children?: ReactNode;
  back?: { label: string; onClick: () => void };
}) {
  return (
    <div className="page-heading">
      {back && (
        <Button
          variant="ghost"
          size="sm"
          icon="arrow-left"
          className="page-heading__back"
          onClick={back.onClick}
        >
          {back.label}
        </Button>
      )}
      <div className="page-heading__row">
        <div className="page-heading__text">
          <span className="az-eyebrow accent">{eyebrow}</span>
          <h1 tabIndex={-1}>{title}</h1>
          {text && <p>{text}</p>}
        </div>
        <div className="page-heading__actions">{children}</div>
      </div>
    </div>
  );
}
export function Card({
  title,
  actions,
  children,
  flush,
  className = "",
}: {
  title?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  flush?: boolean;
  className?: string;
}) {
  return (
    <section
      className={`az-card card${flush ? " card--flush" : ""} ${className}`.trim()}
    >
      {(title || actions) && (
        <div className="card__head">
          <h2>{title}</h2>
          {actions}
        </div>
      )}
      {children}
    </section>
  );
}
export function SectionHead({
  title,
  count,
  children,
}: {
  title: string;
  count?: number;
  children?: ReactNode;
}) {
  return (
    <div className="section-head">
      <h2>
        {title}
        {count != null && <Badge>{count}</Badge>}
      </h2>
      <div className="row">{children}</div>
    </div>
  );
}
export function Toolbar({
  children,
  right,
}: {
  children?: ReactNode;
  right?: ReactNode;
}) {
  return (
    <div className="toolbar">
      <div className="row">{children}</div>
      <div className="row muted small">{right}</div>
    </div>
  );
}
export function Mono({
  children,
  muted,
}: {
  children: ReactNode;
  muted?: boolean;
}) {
  return <span className={"mono" + (muted ? " muted" : "")}>{children}</span>;
}
export function Copyable({ text, label }: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <span className="row tight">
      <Mono muted>{text}</Mono>
      <IconButton
        icon={done ? "check" : "copy"}
        label={label ?? "Copy " + text}
        size={13}
        onClick={async (e: { stopPropagation: () => void }) => {
          e.stopPropagation();
          try {
            await navigator.clipboard.writeText(text);
            setDone(true);
            setTimeout(() => setDone(false), 1500);
          } catch {
            /* clipboard unavailable: the value stays selectable */
          }
        }}
      />
    </span>
  );
}
export function Loading({ what }: { what: string }) {
  return (
    <p role="status" className="loading muted">
      <Spinner size={14} /> Loading {what}…
    </p>
  );
}
export function RefreshButton({
  loading,
  reload,
}: {
  loading: boolean;
  reload: () => void;
}) {
  return (
    <Button
      variant="secondary"
      size="sm"
      icon="refresh-cw"
      disabled={loading}
      onClick={reload}
    >
      {loading ? "Loading…" : "Refresh"}
    </Button>
  );
}
export function Unavailable({
  what,
  compact,
}: {
  what: string;
  compact?: boolean;
}) {
  if (compact)
    return (
      <p className="unavailable muted small">
        <Icon name="server-off" size={14} /> {what}: Not available on this
        server yet.
      </p>
    );
  return (
    <EmptyState
      icon="server-off"
      title="Not available on this server yet"
      description={`${what} needs a management API endpoint this server does not provide yet.`}
    />
  );
}
export function Pager({
  cursor,
  next,
  set,
  label,
}: {
  cursor: string;
  next?: string;
  set: (v: string) => void;
  label?: string;
}) {
  return (
    <div className="pager">
      <span className="muted small">{label}</span>
      <div className="row">
        <Button
          size="sm"
          variant="secondary"
          icon="chevron-left"
          disabled={!cursor}
          onClick={() => set("")}
        >
          First page
        </Button>
        <Button
          size="sm"
          variant="secondary"
          iconRight="chevron-right"
          disabled={!next}
          onClick={() => set(next!)}
        >
          Next page
        </Button>
      </div>
    </div>
  );
}

/* ---------- formatting ---------- */
export const uuidPattern =
  "[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}";
export function bytes(n?: number | null) {
  if (n == null || !Number.isFinite(n)) return "—";
  const units = ["B", "KB", "MB", "GB", "TB", "PB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}
export function when(value?: string | null) {
  if (!value) return "—";
  const d = new Date(value);
  return Number.isFinite(d.getTime()) ? d.toLocaleString() : value;
}
export function count(n?: number) {
  return n == null ? "—" : n.toLocaleString();
}
export function useDebounced<T>(value: T, ms = 300) {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}
export const val =
  (set: (v: string) => void) => (e: { target: { value: string } }) =>
    set(e.target.value);
export const chk =
  (set: (v: boolean) => void) => (e: { target: { checked: boolean } }) =>
    set(e.target.checked);
