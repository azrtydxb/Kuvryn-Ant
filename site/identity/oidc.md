# OIDC sign-in

Kuvryn Ant signs browser users in with the OpenID Connect authorization code flow, always with PKCE (S256), `state` and `nonce`. The client can be public (PKCE only) or confidential (PKCE plus a client secret). This page covers both configuration paths, role mapping, and a complete Zitadel setup.

| Item               | Value                                                     |
| ------------------ | --------------------------------------------------------- |
| Login route        | `GET https://ant.example.com/auth/oidc/login`             |
| Redirect URI       | `https://ant.example.com/auth/oidc/callback` (fixed path) |
| Scopes requested   | `openid`, plus any configured extra scopes                |
| ID token algorithm | RS256 only                                                |
| Session            | 8 hours maximum, 30 minutes idle                          |

## How validation works

- The issuer's discovery document (`<issuer>/.well-known/openid-configuration`) is fetched, and its `issuer` must equal the configured issuer **exactly** (a trailing slash matters).
- Discovery cannot widen outbound access. The authorization, token and JWKS URLs that discovery returns must each appear in `allowed_endpoints`, otherwise the provider is rejected. All URLs must be HTTPS. HTTP redirects are refused, requests time out after 10 seconds and responses are capped at 1 MiB.
- The ID token is checked for issuer, audience (the client ID), `azp`, `nonce`, expiry, subject and the access-token hash. JWKS keys are cached and refetched on an unknown key ID.
- The callback must arrive on the configured origin and path; preserve the public `Host` through your proxy.
- Login and callback share a per-client limit of 20 attempts or 5 failures per minute (HTTP 429, `Retry-After: 60`).

The external identity is `(issuer, sub)`. Email addresses are never used to match accounts.

## Option 1: stored provider (API or console)

Configure under Access, Sign-in providers, Add provider, OIDC, or with `/api/v1/identity-providers`. Stored providers support role mapping, connection tests and hot reload. All endpoints require instance `admin`.

### Config fields

| Field               | Required | Meaning                                                                                                                                                         |
| ------------------- | -------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `issuer`            | yes      | HTTPS issuer URL, no query                                                                                                                                      |
| `client_id`         | yes      | OAuth client ID (1 to 256 bytes)                                                                                                                                |
| `allowed_endpoints` | yes      | 1 to 32 exact HTTPS URLs: at least the authorization, token and JWKS endpoints                                                                                  |
| `scopes`            | no       | Up to 16 extra scopes besides `openid` (printable ASCII, no quotes or backslashes, `openid` itself is rejected)                                                 |
| `roles_claim`       | no       | ID-token claim whose values are mapped to roles. The claim can be an array of strings, a string, or an object whose keys are the values (Zitadel project roles) |
| `claim_roles`       | no       | Up to 200 entries `{"value":"<claim value>","role_ids":["<local role UUID>", ...]}` (1 to 32 role IDs each). Required together with `roles_claim`               |
| `groups_claim`      | no       | Claim shown as groups in the sign-in preview (default `groups`). Informational only; it grants nothing                                                          |

The client secret is passed separately as the write-only `secret` field. Omit it for a public PKCE client; on update, omit it to keep the stored value.

### Workflow

1. **Test** a candidate without saving it. The answer lists each check (discovery, JWKS, clock skew) and `suggested_allowed_endpoints` from discovery:

   ```sh
   export ANT=https://ant.example.com
   H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
   curl -fsS -X POST $ANT/api/v1/identity-providers/test -H "$H" -H "$J" -d '{
     "kind":"oidc",
     "config":{"issuer":"https://auth.example.com","client_id":"<CLIENT_ID>",
               "allowed_endpoints":["https://auth.example.com/oauth/v2/authorize"]}}' | jq
   ```

2. **Create** it disabled, with the endpoints you approved:

   ```sh
   curl -fsS -X POST $ANT/api/v1/identity-providers -H "$H" -H "$J" \
     -H 'Idempotency-Key: idp-oidc-1' -d @- <<'JSON' | jq
   {"kind":"oidc","name":"Zitadel","enabled":false,
    "secret":"<CLIENT_SECRET>",
    "config":{"issuer":"https://auth.example.com","client_id":"<CLIENT_ID>",
      "allowed_endpoints":["https://auth.example.com/oauth/v2/authorize",
                           "https://auth.example.com/oauth/v2/token",
                           "https://auth.example.com/oauth/v2/keys"]}}
   JSON
   ```

3. **Preview** a mapping with a pasted ID token. Nothing is verified or stored; the answer shows the subject, the proposed roles, whether the identity would sign in, and problems such as an `iss` or `aud` mismatch:

   ```sh
   curl -fsS -X POST $ANT/api/v1/identity-providers/$IDP/preview -H "$H" -H "$J" \
     -d "{\"sample\":\"$ID_TOKEN\"}" | jq
   ```

4. **Enable** it (`POST /api/v1/identity-providers/{id}/enable` with `If-Match: "<revision>"` and an `Idempotency-Key`). The provider is built first, so a broken configuration fails with 422 and nothing changes. Enabling replaces any other enabled OIDC provider and applies immediately, without a restart.

Other operations: `GET`, `PUT` (with `If-Match`), `DELETE` (with `If-Match`) on `/api/v1/identity-providers/{id}`, and `POST .../disable`. A provider that fails to build at startup is skipped, recorded in `last_error`, and retried; it never stops the server.

## Option 2: mounted file (`ANT_OIDC_CONFIG_FILE`)

The file is read once at startup, shown read-only in the console, and takes precedence over a stored OIDC provider. It is strict JSON: only the keys below, no duplicates, no `null` values, at most 64 KiB, a regular file.

| Key                  | Required | Meaning                                                                                                    |
| -------------------- | -------- | ---------------------------------------------------------------------------------------------------------- |
| `origin`             | yes      | `https://<host>[:port]` with no path; must equal the origin of `ANT_PUBLIC_URL` when that is set           |
| `key`                | yes      | Non-empty identifier for this provider                                                                     |
| `issuer`             | yes      | Issuer URL                                                                                                 |
| `client_id`          | yes      | OAuth client ID                                                                                            |
| `client_secret_file` | no       | Path to a file holding the client secret (one trailing newline is stripped). Omit for a public PKCE client |
| `redirect_uri`       | yes      | Must be exactly `<origin>/auth/oidc/callback`                                                              |
| `allowed_endpoints`  | yes      | 1 to 32 exact HTTPS authorization, token and JWKS URLs                                                     |

```json
{
  "origin": "https://ant.example.com",
  "key": "zitadel",
  "issuer": "https://auth.example.com",
  "client_id": "<CLIENT_ID>",
  "client_secret_file": "/run/ant/oidc/client_secret",
  "redirect_uri": "https://ant.example.com/auth/oidc/callback",
  "allowed_endpoints": [
    "https://auth.example.com/oauth/v2/authorize",
    "https://auth.example.com/oauth/v2/token",
    "https://auth.example.com/oauth/v2/keys"
  ]
}
```

If SAML or LDAP files are also mounted, every file must name the same `origin`. An invalid file stops startup with a generic `browser login startup rejected` error (file contents are never logged).

::: warning File providers have no role mapping
The file format has no `roles_claim` or `claim_roles`. Users of a file-configured provider can sign in only through an explicit identity binding (see [Explicit bindings](#explicit-bindings)). Use a stored provider if you want roles from IdP claims.
:::

### Helm

The chart mounts the whole Secret named in `auth.oidc.existingSecret` at `/run/ant/oidc/` and sets `ANT_OIDC_CONFIG_FILE=/run/ant/oidc/<configKey>` (default key `config.json`). Put the client secret in the same Secret and reference it with an absolute path under `/run/ant/oidc/`.

```sh
kubectl -n ant create secret generic ant-oidc \
  --from-file=config.json=./oidc.json \
  --from-literal=client_secret='<CLIENT_SECRET>'
```

```yaml
# values.yaml
auth:
  oidc:
    existingSecret: ant-oidc
    configKey: config.json
```

The chart routes `/auth` already. See [Helm chart](../install/helm.md#values-reference).

## Role mapping

Roles always come from local roles. With a stored provider:

- An identity with an **explicit binding** keeps the roles an administrator gave its principal; the mapping never touches it.
- An identity with **no binding** whose claim maps to at least one role gets a new user principal and a claim-managed binding on first sign-in (`browser.claim_user_provisioned`). If nothing maps, sign-in is refused (HTTP 401) and nothing is created.
- For claim-managed identities, the mapped memberships are **reconciled on every sign-in**, so a role removed at the IdP is dropped at the next sign-in (`browser.claim_roles_applied`). Existing sessions keep their roles until they expire.
- `role_ids` must be existing local roles; unknown or federation-managed role IDs are rejected with HTTP 400. If a mapping cannot be applied, no session is issued.

Create the target role first and note its UUID (see [Roles and grants](./roles.md)).

## Explicit bindings

For file-configured providers, or to give one person roles by hand:

```sh
# 1. a user principal
USER=$(curl -fsS -X POST $ANT/api/v1/principals -H "$H" -H "$J" \
  -H 'Idempotency-Key: principal-alice-1' \
  -d '{"kind":"user","subject":"alice"}')
PID=$(echo "$USER" | jq -r .id); PREV=$(echo "$USER" | jq -r .revision)

# 2. its roles
curl -fsS -X PUT $ANT/api/v1/principals/$PID/roles -H "$H" -H "$J" \
  -H "If-Match: \"$PREV\"" -H 'Idempotency-Key: principal-alice-roles-1' \
  -d "{\"role_ids\":[\"$ROLE_ID\"]}"

# 3. bind the IdP identity: issuer exactly as configured, subject = the ID token "sub"
curl -fsS -X POST $ANT/api/v1/identity-bindings -H "$H" -H "$J" \
  -H 'Idempotency-Key: binding-alice-1' \
  -d "{\"issuer\":\"https://auth.example.com\",\"subject\":\"<sub>\",\"principal_id\":\"$PID\"}"
```

Bindings can only target enabled `user` principals, and an existing binding is never overwritten. List them with `GET /api/v1/identity-bindings?principal=<id>` and remove with `DELETE /api/v1/identity-bindings/{id}`.

## Zitadel example

This example uses a Zitadel instance at `https://auth.example.com` and Kuvryn Ant at `https://ant.example.com`, and maps a Zitadel project role `ant-readers` to a local role.

### In Zitadel

1. **Project.** Create a project (for example `kuvryn-ant`). Under its Roles, add `ant-readers` (and any other roles you want to map). In the project settings enable **Assert Roles on Authentication**. Note the project's resource ID.
2. **Application.** In the project, create a new application of type **Web**. Choose the authentication method:
   - **PKCE**: public client, no secret. Kuvryn Ant always uses PKCE, so this works without `secret` / `client_secret_file`.
   - **Code**: confidential client with a client secret. Kuvryn Ant sends the secret in addition to PKCE.
3. **Redirect URI:** `https://ant.example.com/auth/oidc/callback`. Kuvryn Ant does not use a post-logout redirect.
4. **Token settings** of the application: enable **User roles inside ID Token**, so the roles claim is present in the ID token that Kuvryn Ant reads. Kuvryn Ant reads only the ID token (not userinfo or the access token).
5. Note the **Client ID** (and the client secret for the Code method).
6. **Authorizations:** grant the project role `ant-readers` to a test user.

Zitadel's issuer is the instance URL, and its endpoints are normally `<issuer>/oauth/v2/authorize`, `<issuer>/oauth/v2/token` and `<issuer>/oauth/v2/keys`. Confirm them with the discovery document or the `suggested_allowed_endpoints` of the test call:

```sh
curl -fsS https://auth.example.com/.well-known/openid-configuration \
  | jq '{issuer, authorization_endpoint, token_endpoint, jwks_uri}'
```

### In Kuvryn Ant (stored provider with role mapping)

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# local role that mapped users receive (grants: see the Roles page)
ROLE_ID=$(curl -fsS -X POST $ANT/api/v1/roles -H "$H" -H "$J" \
  -H 'Idempotency-Key: role-sso-readers-1' -d '{"name":"sso-readers"}' | jq -r .id)

PROJECT=<zitadel project resource ID>
IDP=$(curl -fsS -X POST $ANT/api/v1/identity-providers -H "$H" -H "$J" \
  -H 'Idempotency-Key: idp-zitadel-1' -d @- <<JSON | jq -r .id
{"kind":"oidc","name":"Zitadel","enabled":false,
 "secret":"<CLIENT_SECRET, omit for PKCE>",
 "config":{
   "issuer":"https://auth.example.com",
   "client_id":"<CLIENT_ID>",
   "allowed_endpoints":["https://auth.example.com/oauth/v2/authorize",
                        "https://auth.example.com/oauth/v2/token",
                        "https://auth.example.com/oauth/v2/keys"],
   "scopes":["urn:zitadel:iam:org:projects:roles"],
   "roles_claim":"urn:zitadel:iam:org:project:$PROJECT:roles",
   "claim_roles":[{"value":"ant-readers","role_ids":["$ROLE_ID"]}]}}
JSON
)
```

Zitadel's project roles claim is an object whose keys are the role names (`{"ant-readers": {"<org id>": "<org domain>"}}`); Kuvryn Ant takes the keys as the values to map.

Then:

1. Paste an ID token of the test user into the preview (`POST /api/v1/identity-providers/$IDP/preview`) or the console's preview, and confirm `proposed_roles` contains `sso-readers` and `would_sign_in` is true.
2. Enable the provider (`POST /api/v1/identity-providers/$IDP/enable` with the current revision in `If-Match`).
3. Sign in from a private browser window: the console shows a "Sign in with Zitadel" button. Expect the mapped role only, and a `browser.claim_user_provisioned` audit event.
4. Negative checks: a user without the project role is refused (HTTP 401, no principal is created); removing the role in Zitadel and signing in again drops it.

### In Kuvryn Ant (Helm file, explicit bindings)

Use the [file shown above](#option-2-mounted-file-ant-oidc-config-file) with Zitadel's client ID and endpoints, mount it with `auth.oidc.existingSecret`, and create an [explicit binding](#explicit-bindings) per user with `issuer` `https://auth.example.com` and the user's Zitadel `sub` (the user ID).

## Troubleshooting

| Symptom                                                      | Likely cause                                                                                                                                                                  |
| ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Provider rejected with "unapproved OIDC discovery endpoints" | An authorization, token or JWKS URL from discovery is missing from `allowed_endpoints`, or the discovered `issuer` differs from the configured one (check the trailing slash) |
| 401 after the IdP redirects back                             | No binding and no mapped role for this identity; check the preview                                                                                                            |
| Callback fails behind a proxy                                | The callback host or path differs from `ANT_PUBLIC_URL`; forward the original `Host`                                                                                          |
| Startup fails with "browser login startup rejected"          | Invalid file: unknown key, `redirect_uri` not `<origin>/auth/oidc/callback`, or `origin` differs from `ANT_PUBLIC_URL`                                                        |
| 429 on login                                                 | Per-client login limit (20 attempts or 5 failures per minute)                                                                                                                 |

::: warning Not validated against a live IdP in the repository tests
The OIDC implementation is tested against a local signed test issuer. The Zitadel steps follow the repository's validation runbook; record your own sign-in test before relying on a provider in production.
:::
