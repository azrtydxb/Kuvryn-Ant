# Service accounts and tokens

Applications, CI and package clients authenticate with tokens. Kuvryn Ant has three kinds, all random 32-byte secrets of which only a SHA-256 verifier is stored, shown once at creation:

| Kind                  | Format                                      | Belongs to            | Accepted on                               | Created with                                                                                        |
| --------------------- | ------------------------------------------- | --------------------- | ----------------------------------------- | --------------------------------------------------------------------------------------------------- |
| Service token         | `ant_` followed by 43 base64url characters  | a `service` principal | `/api/v1`, `/mcp`, package protocols      | `POST /api/v1/principals/{id}/tokens`, `ant bootstrap`, `ant bootstrap-token`, GitHub OIDC exchange |
| Personal access token | `antp_` followed by 43 base64url characters | a `user` principal    | Package protocols only                    | `POST /api/v1/auth/personal-access-tokens`                                                          |
| Bootstrap token       | a service token                             | `bootstrap-admin`     | Everything a service token is accepted on | `ant bootstrap`, `ant bootstrap-token`                                                              |

In examples on this site a token is written as `$ANT_TOKEN` or `<TOKEN>`.

Every token carries **scopes**. What a token can do is the intersection of its principal's [role grants](./roles.md) and its scopes: a scope never adds permissions the principal does not hold.

## Service accounts

A service account is a principal of kind `service`. Create it, give it roles, then issue tokens:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

SVC=$(curl -fsS -X POST $ANT/api/v1/principals -H "$H" -H "$J" \
  -H 'Idempotency-Key: principal-ci-build' -d '{"kind":"service","subject":"ci-build"}')
SVC_ID=$(echo "$SVC" | jq -r .id)

curl -fsS -X PUT $ANT/api/v1/principals/$SVC_ID/roles -H "$H" -H "$J" \
  -H "If-Match: \"$(echo "$SVC" | jq -r .revision)\"" -H 'Idempotency-Key: principal-ci-build-roles' \
  -d "{\"role_ids\":[\"$ROLE_ID\"]}"
```

`subject` is a free-form name (1 to 200 characters). Creating principals and issuing service tokens require instance `admin`. In the console: Access, then Tokens.

## Service tokens

`POST /api/v1/principals/{id}/tokens`

| Field                | Required | Meaning                                                                                            |
| -------------------- | -------- | -------------------------------------------------------------------------------------------------- |
| `scopes`             | yes      | 1 to 64 scope objects                                                                              |
| `scopes[].actions`   | yes      | 1 to 6 of `read`, `publish`, `delete`, `configure`, `replicate`, `admin`                           |
| `scopes[].feed_id`   | no       | Limit the scope to one feed (must exist). Omitted means all feeds                                  |
| `scopes[].namespace` | no       | Limit the scope to one namespace (exact match, up to 200 characters). Omitted means all namespaces |
| `expires_at`         | yes      | RFC 3339 time in the future and at most one year away                                              |

The principal must be an enabled `service` principal. **Do not send an `Idempotency-Key`**: issuance rejects it, because retrying must never return the same secret again.

```sh
NPM=$(curl -fsS "$ANT/api/v1/feeds?prefix=npm" -H "$H" | jq -r '.items[] | select(.slug=="npm") | .id')
curl -fsS -X POST $ANT/api/v1/principals/$SVC_ID/tokens -H "$H" -H "$J" -d @- <<JSON
{"scopes":[{"feed_id":"$NPM","actions":["read","publish"]}],
 "expires_at":"$(date -u -d '+90 days' +%Y-%m-%dT%H:%M:%SZ)"}
JSON
```

```json
{
  "id": "<token UUID>",
  "secret": "ant_<43 characters>",
  "expires_at": "2027-01-18T10:00:00Z"
}
```

Store `secret` immediately; it cannot be retrieved again. (On macOS use `date -u -v+90d +%Y-%m-%dT%H:%M:%SZ`.)

How scopes apply: a request succeeds when the roles allow it **and** at least one scope matches the feed (or has no `feed_id`), the namespace (or has no `namespace`) and the action. Instance-level operations have no feed, so a scope that names a `feed_id` never matches them; give administrative tokens a scope without `feed_id`.

### List and revoke

```sh
# metadata only: scopes, status (active, expired, revoked), created, expires, last used
curl -fsS "$ANT/api/v1/tokens?principal=$SVC_ID&status=active" -H "$H" | jq '.items'

# revoke immediately
curl -fsS -X DELETE $ANT/api/v1/tokens/$TOKEN_ID -H "$H" -H 'Idempotency-Key: revoke-1'
```

`last_used_at` is updated at most once per minute. Revocation is immediate. Tokens of a disabled principal are rejected.

## Personal access tokens

Personal access tokens (PATs) let a signed-in **user** (local, OIDC, SAML or LDAP) give package clients a credential tied to their own permissions. They are created in the console under Settings, Tokens, from a browser session; machine tokens, including PATs, cannot create PATs.

| Field             | Meaning                                                                                                        |
| ----------------- | -------------------------------------------------------------------------------------------------------------- |
| `name`            | 1 to 128 characters                                                                                            |
| `scopes`          | 1 to 64 scopes; each has `actions` (1 to 3 of `read`, `publish`, `delete`), optional `feed_id` and `namespace` |
| `expires_in_days` | 1 to 365                                                                                                       |

The response contains the secret once, in `token` (`antp_...`). A user can hold at most 50 active PATs. The list (`GET /api/v1/auth/personal-access-tokens`) shows scopes, last use, expiry and revocation; `DELETE /api/v1/auth/personal-access-tokens/{id}` revokes one immediately, including for uploads in progress. These endpoints act only on the caller's own tokens.

PATs are accepted only on package protocol routes (`/feeds/...` and `/v2/...`), as Bearer or as the password of HTTP Basic auth with any username, when the feed's [client auth policy](./client-auth.md) allows `pat`. They are not accepted on `/api/v1` or `/mcp`.

## Bootstrap token

`ant bootstrap [name]` initializes an empty database: it creates the instance (name defaults to `local`), the service principal `bootstrap-admin` with a `bootstrap-administrator` role holding every action on every resource kind, and prints a 24-hour service token once:

```sh
ant bootstrap local
```

```json
{
  "instance_id": "<UUID>",
  "principal_id": "<UUID>",
  "token_id": "<UUID>",
  "token": "ant_<43 characters>",
  "expires_at": "<RFC 3339>"
}
```

It fails if the database already has an instance. A server that started on an empty database notices the bootstrap and starts serving without a restart.

`ant bootstrap-token` issues a new 24-hour token for `bootstrap-admin` on an already bootstrapped instance, with the same output. Both commands need the database configuration (`ANT_DATABASE_URL` and the usual `ANT_*` settings) and are authorized by access to it. See the [CLI reference](../reference/cli.md#bootstrap).

::: tip
Use the bootstrap token to create real administrators (an SSO mapping, local users or a service account with a narrower role), then let it expire. `bootstrap-token` remains available for recovery.
:::

## Using tokens

| Where           | How                                                                                                                        |
| --------------- | -------------------------------------------------------------------------------------------------------------------------- |
| Management API  | `Authorization: Bearer $ANT_TOKEN` (service tokens only)                                                                   |
| MCP (`/mcp`)    | `Authorization: Bearer $ANT_TOKEN` (service tokens only), see [MCP tools](../reference/mcp.md)                             |
| Package clients | Bearer, or Basic with any username and the token as password; per-client syntax in [Package client auth](./client-auth.md) |
| GitHub Actions  | No stored token: exchange the job's OIDC token, see [GitHub OIDC trust](./github-oidc.md)                                  |

Failed authentication on the management API is rate limited per client and presented username (10 failures per minute), answering 429 with `Retry-After`.
