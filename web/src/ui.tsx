import {
  cloneElement,
  useEffect,
  useId,
  useState,
  type ReactNode,
  type ReactElement,
  type FormEvent,
} from "react";
import { APIError } from "./api";
import { formats, type Feed } from "./generated";
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
export function Failure({ error }: { error: unknown }) {
  return error ? (
    <div className="error" role="alert">
      {error instanceof Error
        ? error.message
        : "Unable to complete the request."}
      {error instanceof APIError && error.requestId && (
        <small>Request ID: {error.requestId}</small>
      )}
    </div>
  ) : null;
}
export function Field({
  label,
  children,
  hint,
}: {
  label: string;
  children: ReactNode;
  hint?: string;
}) {
  const id = useId();
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      {cloneElement(
        children as ReactElement<{ id: string; "aria-describedby"?: string }>,
        { id, "aria-describedby": hint ? id + "-hint" : undefined },
      )}
      {hint && <small id={id + "-hint"}>{hint}</small>}
    </div>
  );
}
export function Format({
  value,
  onChange,
}: {
  value: Feed["format"];
  onChange: (v: Feed["format"]) => void;
}) {
  return (
    <Field label="Format configuration">
      <select
        value={value}
        onChange={(e) => onChange(e.target.value as Feed["format"])}
      >
        {formats.map((f) => (
          <option key={f}>{f}</option>
        ))}
      </select>
    </Field>
  );
}
export function Mutation({
  children,
  run,
  label = "Save changes",
}: {
  children: ReactNode;
  run: () => Promise<unknown>;
  label?: string;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [done, setDone] = useState(false);
  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setBusy(true);
    setError(undefined);
    setDone(false);
    try {
      await run();
      setDone(true);
    } catch (e) {
      setError(e);
    } finally {
      setBusy(false);
    }
  }
  return (
    <form onSubmit={submit}>
      <fieldset disabled={busy}>
        {children}
        <button className="primary" type="submit">
          {busy ? "Saving…" : label}
        </button>
      </fieldset>
      <Failure error={error} />
      {done && <p role="status">Saved successfully.</p>}
    </form>
  );
}
export function Reload({
  loading,
  reload,
}: {
  loading: boolean;
  reload: () => void;
}) {
  return (
    <button type="button" onClick={reload} disabled={loading}>
      {loading ? "Loading…" : "Refresh"}
    </button>
  );
}
