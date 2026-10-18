// These auth routes are owned by the browser-session backend, outside /api/v1.
// Cookies are HttpOnly; neither cookies nor passwords are read into storage.
export type BrowserSession = {
  csrf_token: string;
  session: { PrincipalID: string; InstanceID: string; ExpiresAt: string };
};
export type Providers = { oidc: boolean; saml: boolean; ldap: boolean };
export async function authRequest(path: string, options: RequestInit = {}) {
  let response: Response;
  try {
    response = await fetch(path, {
      ...options,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
    });
  } catch {
    throw Error("Authentication service could not be reached. Try again.");
  }
  return response;
}
export function parseSession(value: unknown): BrowserSession {
  const s = value as BrowserSession;
  if (
    !s ||
    typeof s.csrf_token !== "string" ||
    !/^[A-Za-z0-9_-]{43}$/.test(s.csrf_token) ||
    !s.session ||
    typeof s.session.PrincipalID !== "string" ||
    typeof s.session.InstanceID !== "string" ||
    !Number.isFinite(Date.parse(s.session.ExpiresAt))
  )
    throw Error("The authentication service returned an invalid session.");
  return s;
}
export async function getSession(
  signal?: AbortSignal,
): Promise<BrowserSession | null> {
  const r = await authRequest("/auth/session", { signal });
  if (r.status === 401 || r.status === 404) return null;
  if (!r.ok)
    throw Error(
      "Could not check the browser session. Retry before signing in.",
    );
  try {
    return parseSession(await r.json());
  } catch {
    throw Error("The authentication service returned an invalid session.");
  }
}
export async function getProviders(
  signal?: AbortSignal,
): Promise<Providers | null> {
  const r = await authRequest("/auth/providers", { signal });
  if (!r.ok) return null;
  const p = await r.json().catch(() => null);
  if (
    !p ||
    !Array.isArray(p.providers) ||
    !p.providers.every((v: unknown) => typeof v === "string")
  )
    return null;
  return {
    oidc: p.providers.includes("oidc"),
    saml: p.providers.includes("saml"),
    ldap: p.providers.includes("ldap"),
  };
}
export async function ldapLogin(
  username: string,
  password: string,
  signal?: AbortSignal,
) {
  // A fresh one-use challenge is acquired for every attempt; never reuse on failure.
  const start = await authRequest("/auth/ldap/login", { signal });
  if (start.status === 404) throw Error("LDAP sign-in is not configured.");
  const body = await start.json().catch(() => null);
  if (
    !start.ok ||
    typeof body?.csrf_token !== "string" ||
    !/^[A-Za-z0-9_-]{43}$/.test(body.csrf_token)
  )
    throw Error("Could not start LDAP sign-in. Try again.");
  const request = authRequest("/auth/ldap/login", {
    method: "POST",
    signal,
    headers: {
      "Content-Type": "application/json",
      "X-CSRF-Token": body.csrf_token,
    },
    body: JSON.stringify({ username, password }),
  });
  password = "";
  const result = await request;
  if (!result.ok)
    throw Error(
      result.status === 429
        ? "Too many sign-in attempts. Wait before retrying."
        : "LDAP sign-in failed. Check your credentials and identity binding.",
    );
  // Re-read the authoritative session and CSRF after cookies are issued.
  const session = await getSession(signal);
  if (!session)
    throw Error("Sign-in completed without an active browser session.");
  return session;
}
export async function logout(csrf: string) {
  const r = await authRequest("/auth/logout", {
    method: "POST",
    headers: { "X-CSRF-Token": csrf },
  });
  if (!r.ok)
    throw Error(
      "Local credentials were cleared, but server sign-out could not be confirmed. Retry sign-out.",
    );
}
