# Client authentication for package protocols

Client authentication decides which credentials a feed accepts from package
clients (docker, npm, pip, go, cargo, maven, ...). It is separate from
upstream credentials, which are configured per remote source and only used when
Ant itself talks to an upstream registry.

## Model

Every feed has an effective policy: its own, or the instance default
(Settings, Access). A policy lists the accepted methods:

| Method            | Credential                       | Notes                                                                                                                                                                                                                                                                                                                                      |
| ----------------- | -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `anonymous`       | none                             | GET/HEAD only, only on feeds with anonymous reads enabled, optionally limited to named network groups. Publishing is never anonymous.                                                                                                                                                                                                      |
| `token`           | `ant_...` service token          | Bearer, or Basic password with any username.                                                                                                                                                                                                                                                                                               |
| `basic-directory` | username + password              | Checked against the configured LDAP/AD. The user must have an identity binding; roles come from the binding's memberships, exactly like console login. Positive results are cached for 60 s keyed by a salted hash; five failures per username in ten minutes lock that username, and 30 failures per client per minute block that client. |
| `pat`             | `antp_...` personal access token | Created by SSO users under Settings, Tokens. Bearer, or Basic password with any username.                                                                                                                                                                                                                                                  |
| `github-oidc`     | GitHub Actions OIDC JWT          | Bearer or Basic password, validated against trust rules and exchanged for a short-lived scoped token.                                                                                                                                                                                                                                      |

Until an administrator stores a different default, all methods are accepted,
and `anonymous` still only applies to feeds that already have anonymous reads
on. Existing deployments therefore behave as before.

The middleware (`internal/identity.ClientAuth`) wraps every `/feeds/{slug}/...`
and `/v2/...` route. It classifies the credential, refuses a method the feed
does not accept (403), verifies it, and hands the adapter a verified,
single-use, in-process ticket through the adapter's normal Bearer pipeline. A
request without credentials is passed through and each adapter answers with its
own challenge, unless the policy allows anonymous. An invalid credential never
falls back to anonymous.

Effective permissions are always the principal's role grants. Scopes on a PAT,
a service token or a trust rule only narrow them.

Policy and feed lookups are cached for 5 s, so a policy change can take up to
5 s to apply on another replica (the replica that served the change applies it
immediately).

### Anonymous networks

Network groups (`/api/v1/auth/network-groups`) are named CIDR lists such as
`cluster` or `lan`. A feed policy that allows `anonymous` and names groups
accepts anonymous reads only from those networks; naming none means any
network. A group referenced by a policy cannot be deleted, and a policy whose
group cannot be resolved denies anonymous access.

Only the TCP peer address is used unless the peer is listed in
`ANT_TRUSTED_PROXY_CIDRS` (comma separated). Then the right-most
`X-Forwarded-For` hop that is not itself a trusted proxy is the client.

### Docker login

The registry challenges `GET /v2/` with `WWW-Authenticate: Basic`, so
`docker login` works with a directory user and password, with a PAT or service
token as the password (any username), and with a GitHub OIDC JWT as the
password. There is no separate bearer-token service.

## Personal access tokens

Settings, Tokens (API: `/api/v1/auth/personal-access-tokens`). A signed-in user
picks a name, a feed (or all feeds), operations (`read`, `publish`, `delete`)
and an expiry (at most 365 days). The secret is shown once; only a SHA-256
verifier is stored. The list shows last use, expiry and status. Revoking takes
effect immediately, including for uploads in progress. A user has at most 50
active tokens. Machine tokens, including PATs, cannot create PATs.

## GitHub Actions OIDC

1. Settings, Access: set the audience (for example `https://ant.example.com`).
   An empty audience disables the feature. The issuer is fixed to
   `https://token.actions.githubusercontent.com`; keys come from its JWKS, are
   cached for an hour, and an unknown `kid` triggers at most one refetch per
   30 s so key rotation works.
2. Create a service principal with the roles it needs, then add trust rules
   (`/api/v1/auth/github-oidc/trust-rules`):

   | Field                     | Meaning                                                                                              |
   | ------------------------- | ---------------------------------------------------------------------------------------------------- |
   | `org`                     | Repository owner, exact, case-insensitive.                                                           |
   | `repo`                    | Glob over the repository name without owner.                                                         |
   | `ref`                     | Glob over the full ref. `main` means `refs/heads/main`; use `refs/tags/v*` for tags; `*` is any ref. |
   | `environment`, `workflow` | Globs (`*` and `?`), default `*`.                                                                    |
   | `principal_id`            | Service principal the token acts as.                                                                 |
   | `scopes`, `ttl_seconds`   | Narrowing scopes (feed, operations) and lifetime, 1 to 60 minutes.                                   |

   The oldest enabled rule that matches decides. Only RS256 is accepted; issuer,
   audience, expiry, not-before and signature are all verified.

3. Workflows obtain the OIDC token (`permissions: id-token: write`) with the
   configured audience and either use it directly as the credential, or
   exchange it for a short-lived Ant token:

   ```
   POST /api/v1/auth/github-oidc/exchange
   {"token": "<jwt>"}            (or Authorization: Bearer <jwt>)
   -> {"access_token":"ant_...","token_type":"Bearer","expires_at":"...","trust_rule_id":"..."}
   ```

   Using the JWT directly exchanges once per distinct JWT and reuses the issued
   token until shortly before it expires. Failed exchanges are rate limited per
   client.

### Workflow setup

Get a token once per job and reuse it:

```yaml
permissions:
  id-token: write
  contents: read
steps:
  - name: Ant token
    run: |
      JWT=$(curl -sS -H "Authorization: Bearer $ACTIONS_ID_TOKEN_REQUEST_TOKEN" \
        "$ACTIONS_ID_TOKEN_REQUEST_URL&audience=https://ant.example.com" | jq -r .value)
      ANT=$(curl -sS -X POST https://ant.example.com/api/v1/auth/github-oidc/exchange \
        -H 'Content-Type: application/json' -d "{\"token\":\"$JWT\"}" | jq -r .access_token)
      echo "::add-mask::$ANT"
      echo "ANT_TOKEN=$ANT" >> "$GITHUB_ENV"
```

Replace `ant.example.com` and the feed slug `team` in the examples below. The
registry endpoints are `https://ant.example.com/feeds/<feed>/<format>/`. A
token in `ANT_TOKEN` works as a Bearer token or as a Basic password with any
username.

### docker

```yaml
- run: echo "$ANT_TOKEN" | docker login ant.example.com -u ci --password-stdin
- run: docker pull ant.example.com/team/app:latest
```

### npm (`actions/setup-node`)

```yaml
- uses: actions/setup-node@v4
  with:
    node-version: 22
    registry-url: https://ant.example.com/feeds/team/npm/
  env:
    NODE_AUTH_TOKEN: ${{ env.ANT_TOKEN }}
```

or write `.npmrc` directly:

```
registry=https://ant.example.com/feeds/team/npm/
//ant.example.com/feeds/team/npm/:_authToken=${ANT_TOKEN}
```

### pip (`actions/setup-python`)

```yaml
- uses: actions/setup-python@v5
  with: { python-version: "3.13" }
- run: pip install --index-url "https://ci:${ANT_TOKEN}@ant.example.com/feeds/team/pypi/simple/" mypackage
```

### Go (`actions/setup-go`)

```yaml
- uses: actions/setup-go@v5
  with: { go-version: stable }
- run: |
    printf 'machine ant.example.com login ci password %s\n' "$ANT_TOKEN" > ~/.netrc
    chmod 600 ~/.netrc
    GOPROXY=https://ant.example.com/feeds/team/go GONOSUMDB=* go build ./...
```

### cargo

`.cargo/config.toml`:

```toml
[registries.ant]
index = "sparse+https://ant.example.com/feeds/team/cargo/"
credential-provider = "cargo:token"
```

```yaml
- run: cargo build
  env:
    CARGO_REGISTRIES_ANT_TOKEN: ${{ env.ANT_TOKEN }}
```

### Maven (`actions/setup-java`)

```yaml
- uses: actions/setup-java@v4
  with:
    distribution: temurin
    java-version: 21
    server-id: ant
    server-username: ANT_USER
    server-password: ANT_TOKEN
  env:
    ANT_USER: ci
```

with the repository declared as
`https://ant.example.com/feeds/team/maven/` and id `ant` in the POM or
`settings.xml`.

### Direct JWT, no exchange step

If you prefer not to call the exchange endpoint, pass the OIDC JWT itself as
the credential (`$JWT` instead of `$ANT_TOKEN` above): the server validates it
and applies the same trust rules. The exchange step avoids pasting a long JWT
into tools with small credential limits, and the issued token is revocable by
deleting, disabling or narrowing the trust rule (the rule id is recorded on each
issued token).

## Verification

Tests live in `internal/identity` (every method allow/deny per feed policy,
anonymous CIDR, publish never anonymous, directory lockout and cache, PAT
lifecycle, GitHub OIDC with a fake JWKS issuer including rotation and
algorithm confusion), `internal/management` (policy, network group, PAT and
trust rule APIs against the OpenAPI contract) and `internal/oci` (docker login
flow). Run them with `scripts/test-postgres.sh`.
