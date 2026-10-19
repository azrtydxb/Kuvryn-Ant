import { useEffect, useState, type ReactNode } from "react";
import { Failure } from "./ui";
import {
  MfaRequired,
  getProviders,
  ldapLogin,
  localLogin,
  type BrowserSession,
  type Providers,
} from "./auth";
import { Button, Input, ProductLogo, Spinner } from "./ds/bundle";
import emblem from "./ds/assets/brand/ant-emblem.png";

export function SignIn({
  onSession,
  onToken,
  notice,
}: {
  onSession: (s: BrowserSession) => void;
  onToken: (t: string) => void;
  notice?: ReactNode;
}) {
  const [providers, setProviders] = useState<Providers | null>();
  const [retry, setRetry] = useState(0);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [token, setToken] = useState("");
  const [showToken, setShowToken] = useState(false);
  const [localUser, setLocalUser] = useState("");
  const [localPassword, setLocalPassword] = useState("");
  const [localCode, setLocalCode] = useState("");
  const [needCode, setNeedCode] = useState(false);
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
  const none =
    providers &&
    !providers.oidc &&
    !providers.saml &&
    !providers.ldap &&
    !providers.local;
  const sso = providers && (providers.oidc || providers.saml);
  const passwordForms = providers && (providers.local || providers.ldap);
  return (
    <div className="az-card signin">
      <ProductLogo
        name="Kuvryn"
        sub="Ant"
        pillar="operate"
        layout="horizontal"
        size={44}
        emblem={emblem}
      />
      <div>
        <h1 tabIndex={-1}>Sign in</h1>
        <p className="muted small">
          Browser sessions use secure HttpOnly cookies. Credentials and CSRF
          tokens are never saved to browser storage.
        </p>
      </div>
      {notice}
      {providers === undefined ? (
        <p role="status" className="loading muted">
          <Spinner size={14} /> Checking sign-in providers…
        </p>
      ) : providers === null ? (
        <div className="stack">
          <p className="muted small">
            Provider availability could not be determined.
          </p>
          <Button variant="secondary" onClick={() => setRetry((v) => v + 1)}>
            Retry provider discovery
          </Button>
        </div>
      ) : (
        <>
          {providers.oidc && (
            <a
              className="az-btn az-btn--primary signin__sso"
              href="/auth/oidc/login"
            >
              Sign in with {providers.labels?.oidc || "OIDC"}
            </a>
          )}
          {providers.saml && (
            <a
              className="az-btn az-btn--secondary signin__sso"
              href="/auth/saml/login"
            >
              Sign in with {providers.labels?.saml || "SAML"}
            </a>
          )}
          {sso && passwordForms && <div className="divider">or</div>}
          {none && (
            <p className="muted small">
              No browser sign-in providers are configured on this server.
            </p>
          )}
          {providers.local && (
            <form
              className="stack"
              onSubmit={async (e) => {
                e.preventDefault();
                if (busy) return;
                let submitted = localPassword;
                setLocalPassword("");
                setBusy(true);
                setError(undefined);
                try {
                  const request = localLogin(
                    localUser,
                    submitted,
                    localCode,
                    controller.signal,
                  );
                  submitted = "";
                  const session = await request;
                  if (!controller.signal.aborted) onSession(session);
                } catch (e) {
                  if (e instanceof MfaRequired) setNeedCode(true);
                  if (!controller.signal.aborted) setError(e);
                } finally {
                  if (!controller.signal.aborted) setBusy(false);
                }
              }}
            >
              {providers.ssoMandatory && (
                <p className="muted small">
                  Single sign-on is required; only the break-glass account may
                  use a password.
                </p>
              )}
              <fieldset disabled={busy} className="plain">
                <Input
                  label="Username"
                  required
                  autoComplete="username"
                  maxLength={64}
                  value={localUser}
                  onChange={(e: { target: { value: string } }) =>
                    setLocalUser(e.target.value)
                  }
                />
                <Input
                  label="Password"
                  required
                  type="password"
                  autoComplete="current-password"
                  maxLength={256}
                  value={localPassword}
                  onChange={(e: { target: { value: string } }) =>
                    setLocalPassword(e.target.value)
                  }
                />
                {needCode && (
                  <Input
                    label="Authenticator code"
                    required
                    inputMode="numeric"
                    autoComplete="one-time-code"
                    maxLength={6}
                    value={localCode}
                    onChange={(e: { target: { value: string } }) =>
                      setLocalCode(e.target.value)
                    }
                  />
                )}
                <Button type="submit" variant={sso ? "secondary" : "primary"}>
                  {busy ? "Signing in…" : "Sign in"}
                </Button>
              </fieldset>
              <Failure error={error} />
            </form>
          )}
          {providers.ldap && (
            <>
              {providers.local && <div className="divider">LDAP</div>}
              <form
                className="stack"
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
                <fieldset disabled={busy} className="plain">
                  <Input
                    label="LDAP username"
                    required
                    autoComplete="username"
                    maxLength={256}
                    value={username}
                    onChange={(e: { target: { value: string } }) =>
                      setUsername(e.target.value)
                    }
                  />
                  <Input
                    label="LDAP password"
                    required
                    type="password"
                    autoComplete="off"
                    maxLength={4096}
                    value={password}
                    onChange={(e: { target: { value: string } }) =>
                      setPassword(e.target.value)
                    }
                  />
                  <Button type="submit" variant="secondary">
                    {busy ? "Signing in…" : "Sign in with LDAP"}
                  </Button>
                </fieldset>
                <Failure error={error} />
              </form>
            </>
          )}
        </>
      )}
      <button
        type="button"
        className="linkbtn"
        aria-expanded={showToken}
        onClick={() => setShowToken((v) => !v)}
      >
        {showToken ? "Hide API token" : "Use an API token"}
      </button>
      {showToken && (
        <form
          className="stack"
          onSubmit={(e) => {
            e.preventDefault();
            if (token.trim()) {
              onToken(token.trim());
              setToken("");
            }
          }}
        >
          <p className="muted small">
            For applications and CI. People sign in above. Use an already
            short-lived token. It stays in memory for at most 15 minutes and is
            cleared on sign-out or leaving this page. This does not revoke the
            server token.
          </p>
          <Input
            label="Short-lived API token"
            type="password"
            mono
            required
            autoComplete="off"
            spellCheck={false}
            value={token}
            onChange={(e: { target: { value: string } }) =>
              setToken(e.target.value)
            }
          />
          <Button type="submit" variant="secondary" disabled={busy}>
            Use token
          </Button>
        </form>
      )}
    </div>
  );
}
