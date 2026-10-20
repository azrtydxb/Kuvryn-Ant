# Break-glass access

People normally sign in to Kuvryn Ant through OIDC, SAML or LDAP. The break-glass administrator is a local account that keeps working when single sign-on is misconfigured, expired or down: it is the way back in. Treat it like a root credential. Store its password in a vault, use it only to repair access, and reset it after every use.

See [Identity and access](../identity/index.md) for the sign-in providers and [Local users](../identity/local-users.md) for ordinary username/password accounts.

## What it is

- One account per instance, username `break-glass`, kind `emergency`.
- Password stored as an argon2id hash (64 MiB, 3 passes). Passwords must be 14 to 256 bytes.
- Full administrator grants, bound to the fixed identity `ant-local` / `break-glass`.
- Optional TOTP second factor, enrolled by the account itself. Each code works once.
- Every attempt is audited (`auth.local.*`, `auth.breakglass.*`).
- It is **not** affected by the SSO-mandatory policy and does not depend on any provider being loaded.

## Lockout and rate limits

- Per peer address: 20 login requests per minute, then HTTP 429. Behind a reverse proxy, configure [trusted proxies](../install/ingress-tls.md) so the real client address is used.
- Per account: 5 consecutive failures lock the account for 15 minutes. Each further failure after a lock expires doubles the lock (30 minutes, 1 hour, ...) up to 24 hours. A successful sign-in resets the counter.
- A locked account answers exactly like a wrong password, so the response does not reveal the lock. The audit log records `auth.local.denied` with reason `locked`.
- An administrator can clear a lock in the console (Access, Sign-in providers) or with `POST /api/v1/local-account/unlock`.

## Create or reset the account

Run the command on a host that has the normal server configuration: the database URL and the **root keyring** (`ANT_ROOT_KEY_FILE` and, after a rotation, `ANT_PREVIOUS_ROOT_KEYS_FILE`). Anyone able to do this already controls the deployment; that is the security model.

```sh
ant admin break-glass                                       # generates a password and prints it once
printf '%s\n' "$PASSWORD" | ant admin break-glass --password-stdin
```

In Kubernetes:

```sh
kubectl exec -it deployment/ant-ant -- ant admin break-glass
```

The command prints one JSON line (`created`, and `password` unless it was read from stdin). A reset always:

1. replaces the password hash,
2. clears the failure counter and lockout,
3. removes TOTP (a lost authenticator is recovered by a reset),
4. revokes existing browser sessions of the account,
5. writes an `auth.breakglass.reset` (or `auth.breakglass.created`) audit event.

Without the root keyring the command refuses to run. It does not need the server to be up, and it works with SSO completely broken.

## Signing in

Open `https://ant.example.com/console/`, enter `break-glass` and the password in the username and password form and, if enrolled, the 6-digit code. The code field appears only after the password is accepted. The session uses the same HttpOnly cookies and CSRF protection as any other browser session.

## Recovering from a broken SSO configuration

1. Sign in as break-glass.
2. Open Access, Sign-in providers. A provider with an **Error** status shows the failure class. Use **Edit**, then **Test connection**, which reports each stage: discovery, JWKS and clock skew (OIDC), metadata and certificate expiry (SAML), or TCP, TLS, bind and search bases (LDAP).
3. Fix the setting and save, or **Disable** the provider. Changes apply at once without a restart.
4. If SSO-mandatory is on and you must turn it off, switch it off on the same page. The break-glass account is exempt from the policy, so it can always do this.

A provider that cannot be built is skipped and retried every minute; it never stops the server or the other providers. Providers configured from mounted files (`ANT_OIDC_CONFIG_FILE`, `ANT_SAML_CONFIG_FILE`, `ANT_LDAP_CONFIG_FILE`) are shown read-only and win over a stored provider of the same type. To change those, edit the mounted file and restart.

## SSO mandatory

Turning on **Require SSO** disables local password sign-in for every account except break-glass. The API refuses to enable it unless at least one provider is enabled (stored or file-configured) and the break-glass account exists, and it refuses to disable or delete the last enabled provider while it is on. Check that the break-glass password works before you enable it.

## Two-factor authentication

While signed in as break-glass, open Access, Sign-in providers, Emergency administrator, **Set up two-factor**. Only the break-glass account can change its own second factor; no other administrator can weaken it. If the authenticator is lost, reset the account on the host.

## After using break-glass

1. Reset the password (`ant admin break-glass`) and store the new value.
2. Review the audit log for `auth.breakglass.login` and `auth.local.*` events.
3. Confirm the cause of the original SSO failure is fixed and re-run the provider test.

## Audit events

| Event                                               | Result  | Meaning                                                         |
| --------------------------------------------------- | ------- | --------------------------------------------------------------- |
| `auth.breakglass.login`                             | success | Break-glass sign-in                                             |
| `auth.local.success`                                | success | Other local account sign-in                                     |
| `auth.local.failure`                                | failure | Bad password or TOTP, with the failed-attempt count             |
| `auth.local.denied`                                 | denied  | Locked account, or SSO mandatory                                |
| `auth.local.mfa_required`                           | denied  | Correct password, no code supplied                              |
| `auth.breakglass.created` / `auth.breakglass.reset` | success | CLI create or reset                                             |
| `identity_provider.*`                               | success | Provider create, update, enable, disable, delete, test, preview |
| `auth.denied`                                       | denied  | Rate-limited 401/403 on management, MCP or package protocols    |

## Local administrator accounts from the host

The same host-level command can create an ordinary local account, optionally with full administrator rights:

```sh
ant admin local-user USERNAME [--admin] [--password-stdin]
```

It creates the account (with `--admin`, also the full-grant `local-administrator` role) or, if it already exists, resets its password and prints it once. Unlike break-glass, local accounts are disabled while SSO is mandatory. Through the API (instance admin): `GET|POST /api/v1/local-accounts`, `PUT /api/v1/local-accounts/{username}/password`, `DELETE /api/v1/local-accounts/{username}`. A new account has no roles; assign them with `PUT /api/v1/principals/{id}/roles`.
