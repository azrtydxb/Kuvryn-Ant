# Break-glass administrator runbook

Kuvryn Ant normally signs people in through OIDC, SAML or LDAP. The
break-glass administrator is a local account that keeps working when single
sign-on is misconfigured, expired or down. It is the way back in. Treat it like a
root credential: store its password in a vault, use it only to repair access, and
reset it after every use.

## What it is

- One account per instance, username `break-glass`, kind `emergency`.
- Password stored as an argon2id hash (64 MiB, 3 passes). Minimum 14 characters.
- Full administrator grants, bound to the fixed identity `ant-local` / `break-glass`.
- Optional TOTP second factor, enrolled by the account itself. Codes work once.
- Every attempt is audited (`auth.local.*`, `auth.breakglass.*`).
- It is **not** affected by SSO-mandatory policy and does not depend on any
  provider being loaded.

## Lockout and rate limits

- Per peer address: 20 login requests per minute, then HTTP 429.
- Per account: 5 consecutive failures lock the account for 15 minutes. Each
  further failure after the lock expires doubles the lock (30 min, 1 h, ...) up to
  24 hours. A successful sign-in resets the counter.
- A locked account answers exactly like a wrong password, so the response does not
  reveal the lock. The audit log records `auth.local.denied` with reason `locked`.
- An administrator can clear a lock from Access, Sign-in providers, or with
  `POST /api/v1/local-account/unlock`.

## Create or reset (on the host)

Run on a host that has the normal server configuration: the database URL and the
**root keyring** (`ANT_ROOT_KEY_FILE` and, after a rotation, the previous-keys
file). Anyone able to do this already controls the deployment; that is the model.

```
ant admin break-glass                    # generates a password and prints it once
printf '%s\n' "$PASSWORD" | ant admin break-glass --password-stdin
```

The command prints one JSON line (`created`, `password` unless read from stdin).
A reset always:

1. replaces the password hash,
2. clears the failure counter and lockout,
3. removes TOTP (a lost authenticator is recovered by reset),
4. revokes existing browser sessions of the account,
5. writes an `auth.breakglass.reset` (or `.created`) audit event.

Without the root keyring the command refuses to run. It does not need the server
to be up, and it works with SSO completely broken.

## Signing in

Open `/console/`, choose **Emergency access**, enter `break-glass`, the password
and, if enrolled, the 6-digit code. The code field appears only after the password
is accepted. The session uses the same HttpOnly cookies and CSRF protection as any
other browser session.

## Recovering from a broken SSO configuration

1. Sign in as break-glass (above).
2. Open Access, Sign-in providers. A provider with a red **Error** status shows
   the failure class. Use **Edit**, then **Test connection**, which reports each
   stage: discovery, JWKS and clock skew (OIDC), metadata and certificate expiry
   (SAML), or TCP, TLS, bind and search bases (LDAP).
3. Fix the setting and save, or **Disable** the provider. Changes apply at once
   without a restart.
4. If SSO-mandatory is on and you must turn it off, switch it off in the same page
   (the break-glass account is exempt from the policy, so it can always do this).

A provider that cannot be built is skipped and retried every minute; it never
stops the server or the other providers. Providers configured from mounted files
(`ANT_OIDC_CONFIG_FILE`, `ANT_SAML_CONFIG_FILE`, `ANT_LDAP_CONFIG_FILE`) are shown
read-only and win over a stored provider of the same type. To change those, edit
the mounted file and restart.

## SSO mandatory

Turning on **Require SSO** disables local password sign-in for every account
except break-glass. The API refuses to enable it unless at least one provider is
enabled (stored or file-configured) and the break-glass account exists, and it
refuses to disable or delete the last enabled provider while it is on. Check the
break-glass password works before enabling.

## Two-factor (TOTP)

While signed in as break-glass, use Access, Sign-in providers, Emergency
administrator, **Set up two-factor**. Only the break-glass account can change its
own second factor; no other administrator can weaken it. If the authenticator is
lost, reset the account on the host as above.

## After using break-glass

1. Reset the password (`ant admin break-glass`) and store the new value.
2. Review the audit log for `auth.breakglass.login` and `auth.local.*` events.
3. Confirm the cause of the original SSO failure is fixed and re-run the provider
   test.

## Related audit events

| Event                                | Result  | Meaning                                                         |
| ------------------------------------ | ------- | --------------------------------------------------------------- |
| `auth.breakglass.login`              | success | break-glass sign-in                                             |
| `auth.local.success`                 | success | other local account sign-in                                     |
| `auth.local.failure`                 | failure | bad password or TOTP, with failed-attempt count                 |
| `auth.local.denied`                  | denied  | locked account or SSO mandatory                                 |
| `auth.local.mfa_required`            | denied  | correct password, no code supplied                              |
| `auth.breakglass.created` / `.reset` | success | CLI create or reset                                             |
| `identity_provider.*`                | success | provider create, update, enable, disable, delete, test, preview |
| `auth.denied`                        | denied  | rate-limited 401/403 on management, MCP or package protocols    |
