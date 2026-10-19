// These auth routes are owned by the browser-session backend, outside /api/v1.
// Cookies are HttpOnly; neither cookies nor passwords are read into storage.
export type BrowserSession = {
  csrf_token: string;
  session: { PrincipalID: string; InstanceID: string; ExpiresAt: string };
};
export type Providers = {
  oidc: boolean;
  saml: boolean;
  ldap: boolean;
  // Username/password sign-in (local accounts, including break-glass).
  local?: boolean;
  ssoMandatory?: boolean;
  // Administrator-chosen display names of the active providers, by protocol.
  labels?: Partial<Record<"oidc" | "saml" | "ldap", string>>;
};
// A request that never settles (stalled gateway/pod) must not leave the
// console on "Checking your browser session…" forever.
export const AUTH_TIMEOUT_MS = 10_000;
export async function authRequest(path: string, options: RequestInit = {}) {
  let response: Response;
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), AUTH_TIMEOUT_MS);
  const parent = options.signal;
  const onParent = () => controller.abort();
  if (parent?.aborted) controller.abort();
  else parent?.addEventListener("abort", onParent, { once: true });
  try {
    response = await fetch(path, {
      ...options,
      signal: controller.signal,
      credentials: "same-origin",
      cache: "no-store",
      redirect: "error",
    });
  } catch {
    throw Error("Authentication service could not be reached. Try again.");
  } finally {
    clearTimeout(timer);
    parent?.removeEventListener("abort", onParent);
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
// Display names are administrator-chosen text; keep only short plain strings.
function parseLabels(value: unknown): Providers["labels"] {
  const out: NonNullable<Providers["labels"]> = {};
  if (!value || typeof value !== "object") return out;
  for (const k of ["oidc", "saml", "ldap"] as const) {
    const v = (value as Record<string, unknown>)[k];
    if (typeof v === "string" && v.trim() && v.length <= 64) out[k] = v.trim();
  }
  return out;
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
    local: p.local === true,
    ssoMandatory: p.sso_mandatory === true,
    labels: parseLabels(p.labels),
  };
}
export class MfaRequired extends Error {
  constructor() {
    super("Enter the 6-digit code from your authenticator app.");
  }
}
// Emergency sign-in for the break-glass administrator. The server answers every
// failure (wrong password, lockout, policy) identically; only a correct password
// reveals that a second factor is needed.
export async function localLogin(
  username: string,
  password: string,
  totpCode: string,
  signal?: AbortSignal,
) {
  const request = authRequest("/auth/local/login", {
    method: "POST",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      username,
      password,
      ...(totpCode ? { totp_code: totpCode } : {}),
    }),
  });
  password = "";
  const result = await request;
  if (result.status === 401) {
    const body = await result.json().catch(() => null);
    if (body?.mfa_required === true) throw new MfaRequired();
  }
  if (!result.ok)
    throw Error(
      result.status === 429
        ? "Too many sign-in attempts. Wait before retrying."
        : "Sign-in failed. Check the credentials; repeated failures lock the account.",
    );
  const session = await getSession(signal);
  if (!session)
    throw Error("Sign-in completed without an active browser session.");
  return session;
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
  // A SAML session also signs out at the identity provider: the server answers
  // 200 with a same-origin path whose page posts the signed logout request.
  if (r.status === 200) {
    const body = await r.json().catch(() => null);
    const next = body?.saml_logout;
    if (typeof next === "string" && /^\/auth\/[a-z/_-]+$/.test(next))
      window.location.assign(next);
  }
}
