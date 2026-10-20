# LDAP and Active Directory

Kuvryn Ant can sign browser users in against an LDAP directory or Active Directory, and, for package clients, accept directory usernames and passwords over HTTP Basic (the `basic-directory` [client auth method](./client-auth.md)). Users are identified by an immutable ID (`entryUUID` or `objectGUID`), never by their login name.

## How sign-in works

1. The console fetches a pre-login CSRF state (`GET /auth/ldap/login`, valid 5 minutes, single use, bound to a `SameSite=Strict` cookie).
2. It posts `{"username":"...","password":"..."}` as JSON to `POST /auth/ldap/login` with that state in `X-CSRF-Token` and a same-origin `Origin` header.
3. Kuvryn Ant connects over TLS, binds as the service account, searches the user base for exactly one entry with `objectClass=<user_class>` and `<login_attribute>=<login>` (filter values are escaped), and binds as that user with the supplied password.
4. It reads the user's immutable ID and resolves group membership by searching the group base for `objectClass=<group_class>` entries whose `<member_attribute>` contains the user's DN, following nested groups when configured.
5. The external identity is `(provider_id, base64url(user ID))`. Roles come from an explicit binding or the provider's `group_roles` mapping.

Every failure gives one generic HTTP 401 with no directory error text. Login is limited to 20 attempts per minute per client address.

## Connection and TLS

- `url` is `ldaps://host[:port]` (TLS from the start) or `ldap://host[:port]`, which **always** upgrades with StartTLS. There is no plaintext fallback: a server that refuses StartTLS fails.
- Certificates are always verified, including the host name. Give the directory's CA as `ca_pem` (stored provider) or `ca_file` (file) if it is private; otherwise the system roots are used.
- Operations time out after 10 seconds, connections live at most 30 seconds, and there is no connection pooling, anonymous bind or credential caching on the browser path.
- Referrals and partial results are rejected; aliases are not dereferenced.

## Group mapping

`group_roles` maps group IDs to local role IDs. Group IDs are the **base64url encoding without padding of the raw ID attribute value** as the directory returns it (`objectGUID` is 16 binary bytes; OpenLDAP and 389 return `entryUUID` as its text form). The easiest way to read the exact IDs is the sign-in preview with a test login, which shows the resolved subject, group IDs and proposed roles without a password:

```sh
curl -fsS -X POST $ANT/api/v1/identity-providers/$IDP/preview -H "$H" -H "$J" \
  -d '{"login":"alice"}' | jq '{identity, proposed_roles, would_sign_in, problems}'
```

For an `entryUUID` in text form you can also compute it locally:

```sh
printf '%s' '6f1e1a3e-0b5c-4f7a-9d1e-3c2b1a0f9e8d' | base64 | tr '+/' '-_' | tr -d '='
```

`group_nesting` controls resolution:

| Value              | Behaviour                                                                                                                                                                                                                 |
| ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `nested` (default) | Groups that are members of groups are followed up to depth 10 (and at most 200 groups). A role mapped to an outer group is inherited by members of inner groups. A cycle or a depth or size overflow refuses the sign-in. |
| `direct`           | Only groups the user is a direct member of. Use it when roles must not leak through nesting.                                                                                                                              |

Mapping rules are the same as for OIDC and SAML: explicit bindings win and are never changed, an identity with no binding and no mapped group is refused and nothing is created, and claim-managed memberships are reconciled at every sign-in, so removing a user from a group drops the role at the next sign-in.

::: warning Limits
Only explicit DN-valued membership edges (`member` or the configured attribute) inside `group_base_dn` are followed. Active Directory `primaryGroupID`, `tokenGroups` and cross-domain referrals are not expanded; do not base authorization on those memberships. The service account must be able to read the whole user and group subtrees, because an ACL-hidden entry looks the same as a missing one.
:::

## Option 1: stored provider (API or console)

Configure under Access, Sign-in providers, Add provider, LDAP, or with `/api/v1/identity-providers` (instance `admin`). The bind password is the write-only `secret` field; it is stored encrypted and re-read for each operation.

| Field                | Required | Meaning                                                                                                                        |
| -------------------- | -------- | ------------------------------------------------------------------------------------------------------------------------------ |
| `provider_id`        | yes      | Stable HTTPS URL used only as the identity namespace (issuer of the external identity), up to 256 bytes. It is never contacted |
| `url`                | yes      | `ldaps://...` or `ldap://...` (StartTLS)                                                                                       |
| `bind_dn`            | yes      | Service account DN                                                                                                             |
| `ca_pem`             | no       | PEM CA bundle for the directory (up to 64 KiB)                                                                                 |
| `user_base_dn`       | yes      | Base DN searched for users                                                                                                     |
| `group_base_dn`      | yes      | Base DN searched for groups                                                                                                    |
| `login_attribute`    | yes      | Attribute matched against the login, for example `uid` or `sAMAccountName`                                                     |
| `user_id_attribute`  | yes      | `entryUUID` or `objectGUID`                                                                                                    |
| `group_id_attribute` | yes      | `entryUUID` or `objectGUID`                                                                                                    |
| `user_class`         | yes      | User object class, for example `inetOrgPerson` or `user`                                                                       |
| `group_class`        | yes      | Group object class, for example `groupOfNames` or `group`                                                                      |
| `member_attribute`   | yes      | DN-valued membership attribute on groups, usually `member`                                                                     |
| `group_roles`        | no       | `[{"group_id":"<base64url ID>","role_ids":["<role UUID>"]}]`, up to 1000 groups                                                |
| `group_nesting`      | no       | `nested` (default) or `direct`                                                                                                 |

Test before saving. `POST /api/v1/identity-providers/test` checks TCP, TLS (reports `tls_mode` `ldaps` or `starttls`), the service bind and read access to both bases:

::: code-group

```sh [OpenLDAP]
curl -fsS -X POST $ANT/api/v1/identity-providers -H "$H" -H "$J" \
  -H 'Idempotency-Key: idp-ldap-1' -d @- <<JSON | jq '{id, revision, last_error}'
{"kind":"ldap","name":"Directory","enabled":false,"secret":"<BIND_PASSWORD>",
 "config":{"provider_id":"https://ldap.example.com/ant",
   "url":"ldaps://ldap.example.com:636",
   "bind_dn":"cn=ant-reader,ou=services,dc=example,dc=com",
   "user_base_dn":"ou=people,dc=example,dc=com",
   "group_base_dn":"ou=groups,dc=example,dc=com",
   "login_attribute":"uid","user_id_attribute":"entryUUID","group_id_attribute":"entryUUID",
   "user_class":"inetOrgPerson","group_class":"groupOfNames","member_attribute":"member",
   "group_roles":[{"group_id":"<base64url group entryUUID>","role_ids":["$ROLE_ID"]}],
   "group_nesting":"nested"}}
JSON
```

```sh [Active Directory]
curl -fsS -X POST $ANT/api/v1/identity-providers -H "$H" -H "$J" \
  -H 'Idempotency-Key: idp-ad-1' -d @- <<JSON | jq '{id, revision, last_error}'
{"kind":"ldap","name":"Active Directory","enabled":false,"secret":"<BIND_PASSWORD>",
 "config":{"provider_id":"https://ad.example.com/ant",
   "url":"ldap://dc1.example.com:389",
   "bind_dn":"CN=svc-ant,OU=Service Accounts,DC=example,DC=com",
   "user_base_dn":"OU=Users,DC=example,DC=com",
   "group_base_dn":"OU=Groups,DC=example,DC=com",
   "login_attribute":"sAMAccountName","user_id_attribute":"objectGUID","group_id_attribute":"objectGUID",
   "user_class":"user","group_class":"group","member_attribute":"member",
   "group_roles":[{"group_id":"<base64url objectGUID>","role_ids":["$ROLE_ID"]}],
   "group_nesting":"direct"}}
JSON
```

:::

Then preview a test login, and enable with `POST /api/v1/identity-providers/{id}/enable` (`If-Match: "<revision>"`). Enabling replaces any other enabled LDAP provider without a restart.

## Option 2: mounted file (`ANT_LDAP_CONFIG_FILE`)

Strict JSON (only these keys, no duplicates or `null`, at most 64 KiB), read at startup, read-only in the console, and it takes precedence over a stored LDAP provider.

| Key                                                                                                           | Meaning                                                                                                 |
| ------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `origin`                                                                                                      | `https://<host>`; must match `ANT_PUBLIC_URL` and other provider files                                  |
| `provider_id`                                                                                                 | Stable HTTPS identity namespace (no query or fragment, up to 256 bytes)                                 |
| `url`, `bind_dn`, `user_base_dn`, `group_base_dn`                                                             | As for stored providers                                                                                 |
| `bind_password_file`                                                                                          | File holding the bind password (one trailing newline stripped, up to 4096 bytes); re-read for each bind |
| `ca_file`                                                                                                     | Optional PEM CA file                                                                                    |
| `login_attribute`, `user_id_attribute`, `group_id_attribute`, `user_class`, `group_class`, `member_attribute` | As for stored providers                                                                                 |

```json
{
  "origin": "https://ant.example.com",
  "provider_id": "https://ldap.example.com/ant",
  "url": "ldaps://ldap.example.com:636",
  "bind_dn": "cn=ant-reader,ou=services,dc=example,dc=com",
  "bind_password_file": "/run/ant/ldap/bind_password",
  "ca_file": "/run/ant/ldap/ca.crt",
  "user_base_dn": "ou=people,dc=example,dc=com",
  "group_base_dn": "ou=groups,dc=example,dc=com",
  "login_attribute": "uid",
  "user_id_attribute": "entryUUID",
  "group_id_attribute": "entryUUID",
  "user_class": "inetOrgPerson",
  "group_class": "groupOfNames",
  "member_attribute": "member"
}
```

```yaml
auth:
  ldap:
    existingSecret: ant-ldap # keys: config.json, bind_password, ca.crt
    configKey: config.json
```

The chart mounts the Secret at `/run/ant/ldap/` and sets `ANT_LDAP_CONFIG_FILE`.

::: warning File providers have no group mapping
The file has no `group_roles` or `group_nesting`. Users sign in only through explicit identity bindings: `POST /api/v1/identity-bindings` with `issuer` = `provider_id` and `subject` = the base64url user ID (read it with the preview of a stored provider, or compute it as above). See [Explicit bindings](./oidc.md#explicit-bindings).
:::

## Directory passwords for package clients

With a feed policy that allows `basic-directory`, package clients can send `username:password` as HTTP Basic. The user must have an identity binding; roles come from the binding's memberships, exactly like console login. Successful checks are cached for 60 seconds (keyed by a salted hash), five failures per username within ten minutes lock that username, and 30 failures per client per minute block that client. See [Package client auth](./client-auth.md).

::: warning Gap
In the current code the `basic-directory` verifier is wired to the LDAP provider from `ANT_LDAP_CONFIG_FILE` only (`cmd/ant/clientauth.go`). A provider stored through the API serves browser sign-in but does not appear to back `basic-directory` for package clients. Prefer tokens for package clients, or verify this in your deployment.
:::

## Troubleshooting

| Symptom                              | Likely cause                                                                                           |
| ------------------------------------ | ------------------------------------------------------------------------------------------------------ |
| Test fails at TLS                    | Unknown CA (set `ca_pem`/`ca_file`), wrong host name in the certificate, or StartTLS refused           |
| Test fails at bind                   | Wrong bind DN or password                                                                              |
| Test fails at search bases           | The service account cannot read `user_base_dn` or `group_base_dn`                                      |
| 401 for a valid user                 | No binding and no mapped group; ambiguous or missing user entry; group cycle or nesting deeper than 10 |
| Role missing for a nested-group user | `group_nesting` is `direct`, or the membership is only via `primaryGroupID`                            |

::: warning Not validated against a live directory in the repository tests
The directory client is tested with fixtures and real TLS handshakes, not an OpenLDAP, 389 or Windows AD conformance run. Record your own test before production use.
:::
