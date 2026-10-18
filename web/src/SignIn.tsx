import { useEffect, useState } from "react";
import { Field, Failure } from "./ui";
import {
  getProviders,
  ldapLogin,
  type BrowserSession,
  type Providers,
} from "./auth";
export function SignIn({
  onSession,
  onToken,
}: {
  onSession: (s: BrowserSession) => void;
  onToken: (t: string) => void;
}) {
  const [providers, setProviders] = useState<Providers | null>();
  const [retry, setRetry] = useState(0);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>();
  const [controller] = useState(() => new AbortController());
  useEffect(() => () => controller.abort(), [controller]);
  useEffect(() => {
    let active = true;
    setProviders(undefined);
    getProviders(controller.signal)
      .then((p) => {
        if (active) setProviders(p);
      })
      .catch(() => {
        if (active) setProviders(null);
      });
    return () => {
      active = false;
    };
  }, [retry, controller]);
  return (
    <section className="card signin">
      <h2>Sign in to administer</h2>
      <p>
        Browser sessions use secure HttpOnly cookies. Credentials and CSRF
        tokens are never saved to browser storage.
      </p>
      {providers === undefined ? (
        <p role="status">Checking sign-in providers…</p>
      ) : providers === null ? (
        <p className="notice">
          Provider availability could not be determined.{" "}
          <button onClick={() => setRetry((v) => v + 1)}>
            Retry provider discovery
          </button>
        </p>
      ) : (
        <>
          <div className="toolbar">
            {providers.oidc ? (
              <a className="button" href="/auth/oidc/login">
                Sign in with OIDC
              </a>
            ) : (
              <span>OIDC not configured</span>
            )}
            {providers.saml ? (
              <a className="button" href="/auth/saml/login">
                Sign in with SAML
              </a>
            ) : (
              <span>SAML not configured</span>
            )}
          </div>
          {providers.ldap ? (
            <details>
              <summary>Sign in with LDAP</summary>
              <form
                onSubmit={async (e) => {
                  e.preventDefault();
                  if (busy) return;
                  let submitted = password;
                  setPassword("");
                  setBusy(true);
                  setError(undefined);
                  try {
                    const request = ldapLogin(
                      username,
                      submitted,
                      controller.signal,
                    );
                    submitted = "";
                    const session = await request;
                    if (!controller.signal.aborted) onSession(session);
                  } catch (e) {
                    if (!controller.signal.aborted) setError(e);
                  } finally {
                    if (!controller.signal.aborted) setBusy(false);
                  }
                }}
              >
                <fieldset disabled={busy}>
                  <Field label="LDAP username">
                    <input
                      required
                      autoComplete="username"
                      maxLength={256}
                      value={username}
                      onChange={(e) => setUsername(e.target.value)}
                    />
                  </Field>
                  <Field label="LDAP password">
                    <input
                      required
                      type="password"
                      autoComplete="off"
                      maxLength={4096}
                      value={password}
                      onChange={(e) => setPassword(e.target.value)}
                    />
                  </Field>
                  <button className="primary">
                    {busy ? "Signing in…" : "Sign in with LDAP"}
                  </button>
                </fieldset>
              </form>
              <Failure error={error} />
            </details>
          ) : (
            <p className="muted">LDAP not configured</p>
          )}
        </>
      )}
      <details>
        <summary>Service token fallback · development / administration</summary>
        <p>
          Use an already short-lived service token. It stays in memory for at
          most 15 minutes and is cleared on sign-out or leaving this page. This
          does not revoke the server token.
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (token.trim()) {
              onToken(token.trim());
              setToken("");
            }
          }}
        >
          <Field label="Short-lived bearer token">
            <input
              type="password"
              required
              autoComplete="off"
              spellCheck={false}
              value={token}
              onChange={(e) => setToken(e.target.value)}
            />
          </Field>
          <button className="primary" disabled={busy}>
            Use token
          </button>
        </form>
      </details>
    </section>
  );
}
