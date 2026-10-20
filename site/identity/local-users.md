# Local users

Local accounts are username and password accounts stored in Kuvryn Ant itself. They sign in through the console's username and password form (`POST /auth/local/login`), independent of any SSO provider. There are two kinds:

- the **break-glass** administrator (`break-glass`, principal kind `emergency`), one per instance, with full grants and optional TOTP. It is covered in [Break-glass access](../operations/break-glass.md);
- **ordinary local users** (principal kind `user`), created by an administrator, holding whatever roles are assigned to them.

Both use the same password storage, lockout and rate limits.

## Passwords and lockout

| Property            | Value                                                                                                                    |
| ------------------- | ------------------------------------------------------------------------------------------------------------------------ |
| Username            | `^[a-z0-9][a-z0-9._-]{0,62}$`; `break-glass` is reserved. Sign-in lowercases and trims the username.                     |
| Password length     | 14 to 256 bytes, no NUL byte                                                                                             |
| Hash                | argon2id (64 MiB, 3 passes)                                                                                              |
| Generated passwords | The CLI generates one when `--password-stdin` is not given and prints it once                                            |
| Per-peer rate limit | 20 login requests per minute per client address, then HTTP 429 with `Retry-After: 60`                                    |
| Account lockout     | 5 consecutive failures lock the account for 15 minutes; each further failure after that doubles the lock, up to 24 hours |
| Locked response     | Identical to a wrong password (HTTP 401); the audit log records `auth.local.denied` with reason `locked`                 |
| Reset on success    | A successful sign-in clears the failure counter                                                                          |

When **SSO mandatory** is on (`PUT /api/v1/identity-policy` with `{"sso_mandatory":true}`), every local account except break-glass is refused at sign-in.

Audit events: `auth.local.success`, `auth.local.failure`, `auth.local.denied`, `auth.local.mfa_required`, `auth.local.user_created`, `auth.local.password_set`, `auth.local.user_deleted`, `auth.local.unlocked`, `auth.local.totp_reset`, `auth.local.totp_enabled`.

## Create or reset a user on the host

`ant admin local-user` runs on a host with the normal server configuration: the database URL and the root keyring (`ANT_ROOT_KEY_FILE` and, after a rotation, `ANT_PREVIOUS_ROOT_KEYS_FILE`). It does not need the server to be running, and it refuses to run without the keyring or before `ant bootstrap`.

```text
ant admin local-user USERNAME [--admin] [--password-stdin]
```

| Flag               | Effect                                                                                                                                             |
| ------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| (none)             | Creates the account with a generated password, or, if it exists, resets its password to a new generated one. The password is printed once.         |
| `--password-stdin` | Reads the password from the first line of standard input (up to 1024 bytes) instead of generating one. The password is then not printed.           |
| `--admin`          | Only when creating: also attaches the `local-administrator` role, which has every action on every resource kind. Rejected for an existing account. |

```sh
# create an administrator with a generated password (printed once)
ant admin local-user alice --admin

# create or reset with a password you supply
printf '%s\n' "$PASSWORD" | ant admin local-user bob --password-stdin
```

The command prints one JSON line:

```json
{
  "username": "alice",
  "created": true,
  "principal_id": "<UUID>",
  "administrator": true,
  "password": "<generated>",
  "note": "shown once; sign in at /console/ with username and password"
}
```

A reset (`"created":false`) replaces the password hash, clears the lockout and revokes the account's browser sessions. An enrolled TOTP factor is kept.

On Kubernetes, run it in the Ant pod, which already has the configuration mounted:

```sh
kubectl -n ant exec -it deploy/ant -- ant admin local-user alice --admin
```

::: tip
The deployment name and namespace depend on your Helm release; adjust `deploy/ant` and `-n ant`.
:::

## Manage users through the API

All local-account endpoints require instance `admin`. Mutations need an `Idempotency-Key` header.

| Operation    | Request                                                                           |
| ------------ | --------------------------------------------------------------------------------- |
| List         | `GET /api/v1/local-accounts` (includes break-glass, lockout and last-login times) |
| Create       | `POST /api/v1/local-accounts` with `{"username":"...","password":"..."}`          |
| Set password | `PUT /api/v1/local-accounts/{username}/password` with `{"password":"..."}`        |
| Delete       | `DELETE /api/v1/local-accounts/{username}`                                        |
| Unlock       | `POST /api/v1/local-accounts/{username}/unlock`                                   |
| Reset TOTP   | `DELETE /api/v1/local-accounts/{username}/totp`                                   |

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

PRINCIPAL=$(curl -fsS -X POST $ANT/api/v1/local-accounts -H "$H" -H "$J" \
  -H 'Idempotency-Key: local-user-carol-1' \
  -d '{"username":"carol","password":"<at least 14 characters>"}' | jq -r .principal_id)
```

An account created through the API has **no roles**. Assign them with `PUT /api/v1/principals/{id}/roles` (see [Roles and grants](./roles.md#assign-roles-to-a-principal)); until then every management request it makes is denied.

Deleting an account removes its password, its `ant-local` sign-in binding and its sessions, and disables the principal. The principal is kept for audit integrity, so the username cannot be reused.

## Two-factor authentication (TOTP)

TOTP uses SHA-1, 6 digits and a 30-second period, accepts one step of clock drift either way, and each code works once. When TOTP is enabled the console asks for the code after the password is accepted (the login endpoint answers `401 {"mfa_required":true}` until a `totp_code` is supplied).

Enrolment endpoints are for the signed-in account itself:

| Operation       | Request                                                            |
| --------------- | ------------------------------------------------------------------ |
| Start           | `POST /api/v1/local-account/totp` returns the `otpauth_uri` once   |
| Confirm         | `POST /api/v1/local-account/totp/confirm` with `{"code":"123456"}` |
| Remove          | `DELETE /api/v1/local-account/totp`                                |
| Show my account | `GET /api/v1/local-account/me`                                     |

The break-glass account can always do this (see [Break-glass access](../operations/break-glass.md)). An ordinary local user can enrol only from a browser session, and only when the instance setting **local-user TOTP** is on: `PUT /api/v1/identity-policy` with `{"sso_mandatory":false,"local_user_totp":true}`, or the switch under Access, Sign-in providers. The setting is off by default. Turning it off stops new enrolments but does not remove factors already enrolled; sign-in keeps asking for the code of any account that has one.

An administrator can never enrol or read another user's factor. If a user loses their device, an administrator removes it with `DELETE /api/v1/local-accounts/{username}/totp` (console: Access, Users, row menu, Reset two-factor). That also revokes the account's browser sessions and is audited as `auth.local.totp_reset`. The break-glass account manages its own factor.

## Unlock

- **Break-glass:** `POST /api/v1/local-account/unlock` (instance admin), or the console (Access, Sign-in providers), or reset it with `ant admin break-glass`.
- **Ordinary users:** `POST /api/v1/local-accounts/{username}/unlock` (instance admin; console: Access, Users, row menu, Unlock account) clears the failure counter and lockout and keeps the password. Setting a new password (`PUT /api/v1/local-accounts/{username}/password` or `ant admin local-user USERNAME`) also clears the lockout. Unlocking is audited as `auth.local.unlocked`.

## Sign in

Open `https://ant.example.com/console/` and use the username and password form. The endpoint accepts only same-origin JSON (`Origin` must equal the configured public origin) and is therefore meant for the console, not for scripts. Applications and CI use [tokens](./tokens.md) instead.

Local sign-in needs a trustworthy origin: `ANT_PUBLIC_URL` must be `https://...`, or `http://` on a loopback address for local development. Otherwise the endpoint answers 503.
