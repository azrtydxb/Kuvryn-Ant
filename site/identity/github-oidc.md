# GitHub OIDC trust

GitHub Actions jobs can authenticate to Kuvryn Ant without any stored secret. The job requests a GitHub OIDC ID token for an audience you choose, and Kuvryn Ant validates it against **trust rules** that map repositories, refs, environments and workflows to a service principal and scopes. The result is a short-lived service token (`ant_...`).

The [`azrtydxb/ant-setup-action`](../ci/github-actions.md) action does all of this for you. This page documents the server side and the raw protocol.

## How a JWT is validated

- Issuer: fixed to `https://token.actions.githubusercontent.com`. Signing keys come from its JWKS, cached for an hour; an unknown key ID triggers at most one refetch per 30 seconds, so GitHub key rotation works.
- Algorithm: RS256 only.
- Audience: must contain the configured audience exactly.
- Expiry and not-before are checked with 60 seconds of leeway.
- The `repository` claim must start with `repository_owner` + `/`.

Then the **oldest enabled rule that matches** decides. All rule fields must match:

| Rule field    | Matched against GitHub claim   | Matching                                                                                                                                                            |
| ------------- | ------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `org`         | `repository_owner`             | Exact, case-insensitive                                                                                                                                             |
| `repo`        | `repository` without the owner | Glob                                                                                                                                                                |
| `ref`         | `ref`                          | Glob over the full ref. A pattern not starting with `refs/` is a branch glob: `main` means `refs/heads/main`. Use `refs/tags/v*` for tags. `*` (default) is any ref |
| `environment` | `environment`                  | Glob, default `*`                                                                                                                                                   |
| `workflow`    | `workflow` (the workflow name) | Glob, default `*`                                                                                                                                                   |

Globs support `*` (any run of characters, including `/`) and `?` (one character).

The issued token belongs to the rule's `principal_id` and carries the rule's `scopes`. Effective access is the principal's role grants intersected with those scopes, so a rule can never exceed what its principal already holds.

## 1. Set the audience

The audience is any string up to 256 characters; the convention (and what `ant-setup-action` requests) is the base URL `https://ant.example.com`, which must then match byte for byte. An empty audience disables the feature.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

curl -fsS -X PUT $ANT/api/v1/auth/github-oidc/config -H "$H" -H "$J" \
  -H 'Idempotency-Key: gh-oidc-audience-1' -d '{"audience":"https://ant.example.com"}'
# GET returns {"issuer":"https://token.actions.githubusercontent.com","audience":"...","revision":N}
```

In the console: Settings, Access.

## 2. Create a service principal with roles

Create a `service` principal and give it a role with the grants your workflows need (for example read on the language feeds and publish on the feed you release to). See [Service accounts and tokens](./tokens.md#service-accounts) and [Roles and grants](./roles.md#example-ci-publisher-for-one-feed).

## 3. Add trust rules

`POST /api/v1/auth/github-oidc/trust-rules` (`GitHubTrustRuleInput`):

| Field          | Required | Default | Meaning                                                                                                     |
| -------------- | -------- | ------- | ----------------------------------------------------------------------------------------------------------- |
| `name`         | yes      |         | Up to 128 characters                                                                                        |
| `org`          | yes      |         | Repository owner                                                                                            |
| `repo`         | yes      |         | Repository name glob (without owner)                                                                        |
| `ref`          | no       | `*`     | Ref glob (see above)                                                                                        |
| `environment`  | no       | `*`     | Environment glob                                                                                            |
| `workflow`     | no       | `*`     | Workflow name glob                                                                                          |
| `principal_id` | yes      |         | An enabled `service` principal                                                                              |
| `scopes`       | yes      |         | 1 to 64 scopes: `actions` (1 to 3 of `read`, `publish`, `delete`), optional `feed_id`, optional `namespace` |
| `ttl_seconds`  | no       | `900`   | Token lifetime, 60 to 3600                                                                                  |
| `enabled`      | no       | `true`  |                                                                                                             |

```sh
NPM=$(curl -fsS "$ANT/api/v1/feeds?prefix=npm" -H "$H" | jq -r '.items[] | select(.slug=="npm") | .id')

# any branch or PR of myorg/myrepo may read the npm feed
curl -fsS -X POST $ANT/api/v1/auth/github-oidc/trust-rules -H "$H" -H "$J" \
  -H 'Idempotency-Key: gh-rule-read-1' -d @- <<JSON
{"name":"myrepo read","org":"myorg","repo":"myrepo","ref":"*",
 "principal_id":"$SVC_ID","scopes":[{"feed_id":"$NPM","actions":["read"]}],"ttl_seconds":900}
JSON

# only tags v* in the release environment may publish
curl -fsS -X POST $ANT/api/v1/auth/github-oidc/trust-rules -H "$H" -H "$J" \
  -H 'Idempotency-Key: gh-rule-release-1' -d @- <<JSON
{"name":"myrepo release","org":"myorg","repo":"myrepo","ref":"refs/tags/v*","environment":"release",
 "principal_id":"$SVC_ID","scopes":[{"feed_id":"$NPM","actions":["read","publish"]}],"ttl_seconds":1800}
JSON
```

::: warning Rule order
Rules are evaluated oldest first and the first match wins. In the example above a tag build in the `release` environment also matches the older `ref: "*"` read rule, so it would get the read-only token. Create the narrower rule first, or make the broad rule exclude what the narrow one covers (for example `ref: "refs/heads/*"`).
:::

Manage rules with `GET /api/v1/auth/github-oidc/trust-rules`, `PUT /api/v1/auth/github-oidc/trust-rules/{id}` (full replacement) and `DELETE /api/v1/auth/github-oidc/trust-rules/{id}`. All configuration endpoints require instance `admin` and an `Idempotency-Key` on mutations. In the console: Settings, Access, Trust rules.

## 4. Exchange in a workflow

`POST /api/v1/auth/github-oidc/exchange` needs no other authentication: the JWT is the credential, sent as `{"token":"<jwt>"}` or as `Authorization: Bearer <jwt>`.

```json
{
  "access_token": "ant_<43 characters>",
  "token_type": "Bearer",
  "expires_at": "2026-10-20T12:15:00Z",
  "trust_rule_id": "<UUID>"
}
```

A raw exchange step, requesting the ID token with the configured audience:

```yaml
permissions:
  id-token: write
  contents: read

jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - name: Kuvryn Ant token
        run: |
          JWT=$(curl -fsS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
            "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ant.example.com" | jq -r .value)
          ANT=$(curl -fsS -X POST https://ant.example.com/api/v1/auth/github-oidc/exchange \
            -H 'Content-Type: application/json' -d "{\"token\":\"$JWT\"}" | jq -r .access_token)
          echo "::add-mask::$ANT"
          echo "ANT_TOKEN=$ANT" >> "$GITHUB_ENV"
      - name: npm via Kuvryn Ant
        run: |
          echo "registry=https://ant.example.com/feeds/npm/npm/" > .npmrc
          echo '//ant.example.com/feeds/npm/npm/:_authToken=${ANT_TOKEN}' >> .npmrc
          npm ci
```

`ACTIONS_ID_TOKEN_REQUEST_URL` already contains a query string, hence `&audience=`. The `id-token: write` permission is required; it is not available to workflows triggered from forks or by Dependabot.

::: tip Use the action
[`azrtydxb/ant-setup-action`](../ci/github-actions.md) performs the same exchange, masks the token and points the job's package managers at your feeds. It requests the audience `https://<host>`.
:::

### Using the JWT directly

Package protocol routes also accept the GitHub JWT itself as the credential (Bearer, or Basic password with any username) when the feed's [client auth policy](./client-auth.md) allows `github-oidc`. The server exchanges it once per distinct JWT and reuses the issued token until shortly before it expires. The explicit exchange is still preferable for tools with small credential limits, and the exchanged token works on `/api/v1` as well.

## Errors

| Status | Meaning                                                                                                                                                                                 |
| ------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 401    | Invalid JWT: signature, issuer, audience, expiry or claims                                                                                                                              |
| 403    | `no trust rule matches this workflow` (also when the rule's principal is no longer an enabled service principal, or a scope's feed no longer exists), or the audience is not configured |
| 404    | The exchange is not available on this process (it does not serve package protocols)                                                                                                     |
| 429    | Too many failed exchanges from this client (20 per minute), `Retry-After: 60`                                                                                                           |
| 503    | Storage or JWKS unavailable                                                                                                                                                             |

Debugging a 403: decode the JWT payload (`echo "$JWT" | cut -d. -f2 | base64 -d 2>/dev/null | jq`) and compare `repository_owner`, `repository`, `ref`, `environment` and `workflow` with your rules. For a pull request the `ref` is `refs/pull/<n>/merge`.

## Revocation

An exchanged token is a normal service token: list it with `GET /api/v1/tokens?principal=<id>` and revoke it with `DELETE /api/v1/tokens/{id}`. Deleting or disabling a trust rule stops new exchanges for it; tokens already issued stay valid until they expire (at most `ttl_seconds`, 1 hour maximum) unless revoked.
