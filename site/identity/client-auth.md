# Package client authentication

Package clients (docker, npm, pip, Maven, Go, Cargo and the rest) authenticate to feed endpoints with a token or, optionally, a directory password. Which credentials a feed accepts is set by its **client authentication policy**. This is separate from upstream credentials, which a remote source uses when Kuvryn Ant itself talks to an upstream registry (see [Feeds and sources](../concepts/feeds-and-sources.md)).

## Credentials

A middleware (`identity.ClientAuth`) wraps every `/feeds/{slug}/...` and `/v2/...` route. It classifies the credential in the `Authorization` header, refuses a method the feed does not accept (HTTP 403), verifies it, and hands the adapter a verified principal.

| Method            | Credential                       | Sent as                                                                                        |
| ----------------- | -------------------------------- | ---------------------------------------------------------------------------------------------- |
| `token`           | Service token `ant_...`          | `Authorization: Bearer <TOKEN>`, or HTTP Basic with any username and the token as password     |
| `pat`             | Personal access token `antp_...` | Bearer, or Basic password with any username                                                    |
| `github-oidc`     | GitHub Actions OIDC JWT          | Bearer, or Basic password with any username; validated against [trust rules](./github-oidc.md) |
| `basic-directory` | LDAP / AD username and password  | HTTP Basic `username:password`                                                                 |
| `anonymous`       | none                             | No `Authorization` header; GET and HEAD only                                                   |

Rules that always apply:

- **Publishing is never anonymous.** Anonymous access requires all of: a GET or HEAD request, a feed with `anonymous_read: true`, a policy that lists `anonymous`, and (if the policy names network groups) a client address inside one of them.
- An invalid credential never falls back to anonymous; it answers 401.
- A request without credentials is passed to the adapter, which answers with its own challenge (`WWW-Authenticate: Basic` for most formats), so clients that only send credentials after a challenge work.
- Effective permissions are always the principal's [role grants](./roles.md), narrowed by the scopes of the token, PAT or trust rule.
- For `basic-directory`, the directory user must have an identity binding; roles come from that binding's memberships. Successful checks are cached for 60 seconds, five failures per username in ten minutes lock that username, and 30 failures per client per minute block the client.
- Failed PAT, JWT and directory attempts are budgeted per client: once blocked the answer is 429 with `Retry-After: 60`.

The basic-directory caveat in [LDAP](./ldap.md#directory-passwords-for-package-clients) applies: it is backed by the file-configured LDAP provider.

## Policies

Every feed has an effective policy: its own, or the instance default.

```json
{
  "methods": ["token", "pat", "github-oidc"],
  "anonymous_network_group_ids": []
}
```

- `methods`: 1 to 5 of `anonymous`, `token`, `basic-directory`, `pat`, `github-oidc`.
- `anonymous_network_group_ids`: up to 32 network group IDs that restrict anonymous reads; empty means any network. Only meaningful with `anonymous` in `methods`.

Until an administrator stores a different instance default, **all methods are accepted**, and `anonymous` still applies only to feeds with `anonymous_read` on.

| Operation                          | Request                                                                         | Permission              |
| ---------------------------------- | ------------------------------------------------------------------------------- | ----------------------- |
| Read the instance default          | `GET /api/v1/auth/policy`                                                       | instance `admin`        |
| Replace the instance default       | `PUT /api/v1/auth/policy`                                                       | instance `admin`        |
| Read a feed's effective policy     | `GET /api/v1/feeds/{id}/auth-policy` (`inherited: true` when the feed has none) | `configure` on the feed |
| Set the feed's own policy          | `PUT /api/v1/feeds/{id}/auth-policy`                                            | `configure` on the feed |
| Remove the feed's policy (inherit) | `DELETE /api/v1/feeds/{id}/auth-policy`                                         | `configure` on the feed |

Mutations need an `Idempotency-Key`. In the console: Settings, Access for the default, and the feed's settings for a per-feed policy.

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'

# instance default: tokens only, no directory passwords, no anonymous
curl -fsS -X PUT $ANT/api/v1/auth/policy -H "$H" -H "$J" -H 'Idempotency-Key: auth-default-1' \
  -d '{"methods":["token","pat","github-oidc"],"anonymous_network_group_ids":[]}'
```

Policy and feed lookups are cached for 5 seconds, so a change can take up to 5 seconds to apply on another replica.

## Network groups

Network groups are named CIDR lists for restricting anonymous reads, for example to the cluster network.

```sh
GROUP=$(curl -fsS -X POST $ANT/api/v1/auth/network-groups -H "$H" -H "$J" \
  -H 'Idempotency-Key: netgroup-cluster-1' \
  -d '{"name":"cluster","cidrs":["10.0.0.0/16"]}' | jq -r .id)

# public mirror feed: anonymous reads from the cluster, tokens from anywhere
FEED=$(curl -fsS "$ANT/api/v1/feeds?prefix=dockerhub" -H "$H" | jq -r '.items[] | select(.slug=="dockerhub") | .id')
curl -fsS -X PUT $ANT/api/v1/feeds/$FEED/auth-policy -H "$H" -H "$J" -H 'Idempotency-Key: dockerhub-auth-1' \
  -d "{\"methods\":[\"anonymous\",\"token\",\"pat\",\"github-oidc\"],\"anonymous_network_group_ids\":[\"$GROUP\"]}"
```

The feed itself must also have `anonymous_read: true`. Names match `^[a-z0-9][a-z0-9_-]{0,62}$`, each group holds 1 to 64 CIDRs. Manage groups with `GET /api/v1/auth/network-groups`, `PUT /api/v1/auth/network-groups/{id}` and `DELETE /api/v1/auth/network-groups/{id}` (instance `admin`). A group referenced by a policy cannot be deleted (409), and a policy whose group cannot be resolved denies anonymous access.

The client address is the TCP peer, unless the peer is listed in `ANT_TRUSTED_PROXY_CIDRS` (comma separated). Then the right-most `X-Forwarded-For` hop that is not itself a trusted proxy is used. Set it to your ingress or load balancer addresses, otherwise every request appears to come from the proxy.

## Per client family

The format pages have complete client configuration; this table shows how each family presents a credential. "Any token" means a service token, PAT or GitHub JWT.

| Client family                                                                                                                                              | How the credential is sent                                                                                                          | Notes                                                                          |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------ |
| [OCI](../formats/oci.md): docker, podman, crane, buildx, containerd                                                                                        | `docker login ant.example.com -u <any> --password-stdin` (Basic); the registry challenges `GET /v2/` with `WWW-Authenticate: Basic` | Any token or a directory password. There is no separate bearer-token service   |
| [Helm](../formats/helm.md)                                                                                                                                 | `helm repo add --username <any> --password <TOKEN>` (Basic); `helm registry login` for OCI charts                                   | Any token                                                                      |
| [npm, pnpm, Yarn](../formats/npm.md)                                                                                                                       | `//ant.example.com/feeds/<feed>/npm/:_authToken=<TOKEN>` (Bearer)                                                                   | Any token. The registry requires an explicit credential on every request       |
| [pip, uv, Poetry, twine](../formats/pypi.md)                                                                                                               | Basic: `https://<user>:<TOKEN>@ant.example.com/feeds/<feed>/pypi/simple/`, `~/.netrc`, keyring or `TWINE_PASSWORD`                  | Any token. The username must be non-empty (any value, for example `__token__`) |
| [Maven, Gradle](../formats/maven.md)                                                                                                                       | Basic: `<server>` username and password in `settings.xml`, or Gradle `credentials {}`                                               | Any token                                                                      |
| [Go](../formats/go.md)                                                                                                                                     | Basic from `~/.netrc` (`machine ant.example.com login <any> password <TOKEN>`)                                                      | Any token                                                                      |
| [Cargo](../formats/cargo.md)                                                                                                                               | `Authorization: <token>` verbatim (no scheme), from `CARGO_REGISTRIES_<NAME>_TOKEN` or `cargo login`                                | Any token (scheme-less value)                                                  |
| [NuGet](../formats/nuget.md)                                                                                                                               | Reads: Basic (`packageSourceCredentials`). Push and delete: `X-NuGet-ApiKey: <TOKEN>`                                               | Any token, including on push and delete                                        |
| [RubyGems](../formats/rubygems.md)                                                                                                                         | `gem push` sends the API key verbatim in `Authorization`; Bundler sends Basic credentials                                           | Any token (raw key or Basic)                                                   |
| [Composer](../formats/composer.md)                                                                                                                         | `auth.json` `http-basic` (Basic) or `bearer`                                                                                        | Any token                                                                      |
| [Ansible Galaxy](../formats/ansible.md)                                                                                                                    | `Authorization: Token <TOKEN>` from `ansible.cfg` `token=`                                                                          | Any token (`Token` scheme)                                                     |
| [pub](../formats/pub.md)                                                                                                                                   | `dart pub token add` (Bearer)                                                                                                       | Any token                                                                      |
| [Hugging Face](../formats/huggingface.md), [Terraform](../formats/terraform.md)                                                                            | Bearer (`HF_TOKEN`; Terraform `credentials` block)                                                                                  | Any token                                                                      |
| [Conda](../formats/conda.md), [Conan](../formats/conan.md), [CRAN](../formats/cran.md), [CocoaPods](../formats/cocoapods.md), [Swift](../formats/swift.md) | Basic password or Bearer, depending on the client                                                                                   | Any token                                                                      |
| [APT](../formats/apt.md), [RPM](../formats/rpm.md), [APK](../formats/apk.md), [P2](../formats/p2.md)                                                       | Basic, typically credentials in the repository URL or an auth file (`auth.conf.d`, `username=`/`password=`)                         | Any token                                                                      |
| [Git LFS](../formats/git-lfs.md)                                                                                                                           | Basic from the Git credential helper (`LFS-Authenticate: Basic`)                                                                    | Any token                                                                      |
| [Raw / WebDAV](../formats/raw.md)                                                                                                                          | Basic (WebDAV clients, curl `-u`) or Bearer                                                                                         | Any token                                                                      |

::: tip Non-standard credential locations
Besides `Authorization: Bearer` and `Basic`, the middleware resolves the `Token` scheme (Ansible Galaxy), a scheme-less `Authorization` value on Cargo and RubyGems feeds, and `X-NuGet-ApiKey` on NuGet push and delete. They resolve exactly like Bearer, so service tokens, PATs and GitHub JWTs all work, with the feed's method policy, scopes, audit and failure rate limits applied. These locations are never treated as a username and password: only Basic carries a directory login.
:::

## Troubleshooting

| Status                                                  | Meaning                                                                                                   |
| ------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| 401                                                     | No credential (the client should retry after the challenge), or an invalid, expired or revoked credential |
| 403 `authentication method not permitted for this feed` | The feed's policy does not list this method                                                               |
| 403 `no trust rule matches this workflow`               | GitHub JWT valid but no trust rule matched                                                                |
| 403 (other)                                             | Authenticated, but the role grants or token scopes do not allow the action on this feed or namespace      |
| 429                                                     | Too many failed attempts from this client; wait for `Retry-After`                                         |
| 503                                                     | Identity storage or the directory is unavailable                                                          |

Anonymous pull fails although `anonymous` is allowed: check the feed's `anonymous_read`, the network group and `ANT_TRUSTED_PROXY_CIDRS`.
