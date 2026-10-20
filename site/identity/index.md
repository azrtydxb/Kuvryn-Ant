# Identity and access

Kuvryn Ant decides who a request comes from (authentication) and what that caller can do (authorization) in one place, `internal/identity`, for the console, the management API, MCP and every package protocol. This page describes the model. The pages after it cover each sign-in method, roles, tokens and package-client authentication in detail.

## Principals

A principal is the identity that roles attach to. Every principal belongs to one instance.

| Kind        | Created by                                                                                                                                             | Signs in with                                                                              |
| ----------- | ------------------------------------------------------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------ |
| `user`      | `POST /api/v1/principals`, a local account (`ant admin local-user` or `POST /api/v1/local-accounts`), or just-in-time provisioning from a role mapping | The browser (local password, OIDC, SAML, LDAP); personal access tokens for package clients |
| `service`   | `POST /api/v1/principals` with `"kind":"service"`; `ant bootstrap` creates the first one (`bootstrap-admin`)                                           | Service tokens (`ant_...`), directly or issued by a GitHub Actions OIDC exchange           |
| `emergency` | `ant admin break-glass` (one per instance, username `break-glass`)                                                                                     | The console username and password form, optionally with TOTP                               |

Service tokens can only be issued to enabled `service` principals. Personal access tokens can only be created by enabled `user` principals. A disabled principal fails every credential it holds.

## Authentication paths

### Browser (console)

All browser routes live under `/auth/` and produce the same kind of session: HttpOnly `__Host-` cookies plus a CSRF token, an 8-hour maximum lifetime and a 30-minute idle timeout. Sessions authenticate `/api/v1` requests that carry no `Authorization` header.

| Method                           | Route                                              | Page                                           |
| -------------------------------- | -------------------------------------------------- | ---------------------------------------------- |
| Local username and password      | `POST /auth/local/login`                           | [Local users](./local-users.md)                |
| OIDC (authorization code + PKCE) | `GET /auth/oidc/login`, `GET /auth/oidc/callback`  | [OIDC](./oidc.md)                              |
| SAML 2.0 (SP-initiated)          | `GET /auth/saml/login`, `POST /auth/saml/callback` | [SAML](./saml.md)                              |
| LDAP / Active Directory          | `GET`/`POST /auth/ldap/login`                      | [LDAP](./ldap.md)                              |
| Sign out                         | `POST /auth/logout`                                | (SAML single logout: [SAML](./saml.md#logout)) |

`GET /auth/providers` tells the sign-in page which protocols are active, whether local sign-in is available and whether SSO is mandatory.

SSO providers can be configured in two ways:

- **Mounted files** (`ANT_OIDC_CONFIG_FILE`, `ANT_SAML_CONFIG_FILE`, `ANT_LDAP_CONFIG_FILE`, mounted by the Helm chart from `auth.<protocol>.existingSecret`). Read at startup, shown read-only in the console, and they take precedence over a stored provider of the same protocol.
- **Stored providers** (`/api/v1/identity-providers`, console Access, Sign-in providers). Validated and built before they are saved, applied without a restart, and the only way to configure claim, attribute or group role mappings.

At most one provider per protocol is active at a time: enabling a stored provider replaces any other enabled provider of the same protocol.

### API, MCP and package clients

| Credential                    | Prefix / shape          | Accepted on                                                                       | Page                                         |
| ----------------------------- | ----------------------- | --------------------------------------------------------------------------------- | -------------------------------------------- |
| Service token                 | `ant_` + 43 characters  | `/api/v1` (Bearer), `/mcp` (Bearer), package protocols (Bearer or Basic password) | [Tokens](./tokens.md)                        |
| Personal access token (PAT)   | `antp_` + 43 characters | Package protocols only (Bearer or Basic password)                                 | [Tokens](./tokens.md#personal-access-tokens) |
| GitHub Actions OIDC JWT       | JWT (`eyJ...`)          | `POST /api/v1/auth/github-oidc/exchange`; package protocols directly              | [GitHub OIDC trust](./github-oidc.md)        |
| Directory username + password | HTTP Basic              | Package protocols, when the feed policy allows `basic-directory`                  | [Package client auth](./client-auth.md)      |
| Bootstrap token               | a service token         | Everything a service token is accepted on                                         | [Tokens](./tokens.md#bootstrap-token)        |

Which of these a feed accepts is set by its [client authentication policy](./client-auth.md).

## Authorization model

Authorization comes only from **persisted roles**. A role holds a list of grants; a principal holds a list of role memberships. Details and examples are in [Roles and grants](./roles.md).

A grant has:

- `kind`: the resource kind, `artifact`, `feed`, `source` or `instance`. Kinds never imply each other: an `instance` grant does not cover feeds, and a `feed` grant does not cover the artifacts in it.
- selectors: `feed_id`, `source_id` and `namespace` narrow the grant; an empty selector means all values.
- `actions`: any of `read`, `publish`, `delete`, `configure`, `replicate`, `admin`. `admin` is a distinct action, not a wildcard.
- `effect`: `allow` or `deny`.

Evaluation rules (from `Policy.Authorize`):

1. A request is allowed only if at least one matching `allow` grant exists in the principal's roles.
2. **Any matching `deny` wins**, including a deny from a different role than the allow.
3. For a token (service token, PAT or a token issued by a GitHub trust rule) the result is then **intersected with the token's scopes**: at least one scope must cover the feed, the namespace and the action. Scopes only ever narrow what the roles grant; a token cannot do more than its principal.
4. Browser sessions are not narrowed by scopes.

`GET /api/v1/permissions/effective` shows the result for a principal and resource, including which roles allow and which deny each action.

## Identity bindings

An external identity is a pair `(issuer, subject)`: the OIDC issuer and `sub`, the SAML IdP entity ID and NameID, the LDAP provider ID and the user's immutable ID, or `ant-local` and the username for local accounts. A binding links that pair to a `user` principal.

- **Explicit bindings** are created by an instance administrator (`POST /api/v1/identity-bindings`) for an existing, enabled user principal. The principal's roles are whatever the administrator assigned.
- **Claim-managed bindings** are created at sign-in by a stored provider's role mapping (`claim_roles` for OIDC and SAML, `group_roles` for LDAP). The identity gets a new user principal holding exactly the mapped roles, and those memberships are reconciled on every later sign-in.

::: tip Login never implicitly creates administrators
A provider can only propose existing **local** roles through an explicit mapping that an administrator configured. An identity with no binding and no mapped role is refused and nothing is created. Explicit bindings and their memberships are never changed by a mapping, an existing principal is never taken over, and group or claim values without a mapping grant nothing. File-configured providers have no mapping fields at all, so they work only with explicit bindings.
:::

## Related pages

- [Break-glass access](../operations/break-glass.md): the emergency administrator that keeps working when SSO is broken.
- [Security model](../about/security.md)
- [Management API](../reference/api.md)
