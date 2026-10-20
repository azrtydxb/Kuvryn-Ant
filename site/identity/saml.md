# SAML sign-in

Kuvryn Ant is a SAML 2.0 service provider (SP) for SP-initiated browser sign-in: a signed `AuthnRequest` goes out over HTTP-Redirect and the IdP's response comes back over HTTP-POST. Single logout is supported over HTTP-POST.

## Endpoints

These routes exist only while a SAML provider is active.

| Route                      | Purpose                                                                                   |
| -------------------------- | ----------------------------------------------------------------------------------------- |
| `GET /auth/saml/login`     | Starts sign-in (signed `AuthnRequest`, HTTP-Redirect binding)                             |
| `POST /auth/saml/callback` | Assertion consumer service (ACS), HTTP-POST binding                                       |
| `GET /auth/saml/metadata`  | SP metadata: entity ID, ACS, single logout endpoint, signing certificate                  |
| `POST /auth/saml/slo`      | SP single logout endpoint: the IdP's `LogoutResponse` or an IdP-initiated `LogoutRequest` |
| `GET /auth/saml/logout`    | Serves the staged, signed `LogoutRequest` once as a self-submitting form to the IdP       |

With the public origin `https://ant.example.com`, the values the IdP needs are:

| IdP setting       | Value                                                                                                    |
| ----------------- | -------------------------------------------------------------------------------------------------------- |
| ACS URL           | `https://ant.example.com/auth/saml/callback` (HTTP-POST)                                                 |
| SP entity ID      | Whatever you configure as `entity_id`; the console proposes `https://ant.example.com/auth/saml/metadata` |
| Audience          | The SP entity ID                                                                                         |
| Single logout URL | `https://ant.example.com/auth/saml/slo` (HTTP-POST)                                                      |
| SP metadata       | `https://ant.example.com/auth/saml/metadata`, or `GET /api/v1/identity-providers/{id}/metadata` (admin)  |

## IdP requirements

- **The assertion itself must be signed.** A signed response envelope alone is not enough. Unsigned or wrongly signed assertions, signature wrapping and duplicate assertions are rejected.
- Exactly one assertion with one bearer subject confirmation, one authentication statement and one audience restriction equal to the SP entity ID.
- No assertion encryption (not supported).
- NameID format persistent (the default) with a stable value. Configure another format only if you must; the NameID is the subject of the external identity.
- Clocks in sync: assertions are validated **without clock-skew tolerance**.
- SP-initiated only. Unsolicited (IdP-initiated) sign-in is not supported; a pending login request lives at most 5 minutes.
- Responses up to 1 MiB of XML.

Assertions and requests are replay-protected with a persistent store, so a response replayed through the browser back button fails.

The external identity is `(IdP entity ID, NameID)`.

## Option 1: stored provider (API or console)

Configure under Access, Sign-in providers, Add provider, SAML, or with `/api/v1/identity-providers` (instance `admin`).

### Config fields

| Field                | Required       | Meaning                                                                                                                                                                                                             |
| -------------------- | -------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `entity_id`          | yes            | SP entity ID (up to 2048 bytes)                                                                                                                                                                                     |
| `idp_entity_id`      | yes            | IdP entity ID, exactly as in its metadata                                                                                                                                                                           |
| `idp_metadata_xml`   | yes            | Pasted IdP metadata XML (up to 256 KiB). It is never fetched from a URL                                                                                                                                             |
| `roles_attribute`    | no             | Assertion attribute whose values `claim_roles` maps to local roles                                                                                                                                                  |
| `claim_roles`        | no             | `[{"value":"<attribute value>","role_ids":["<role UUID>"]}]`, required together with `roles_attribute`                                                                                                              |
| `groups_attribute`   | no             | Attribute recorded as the identity's groups (used by federated group bindings); grants nothing on its own                                                                                                           |
| `email_attribute`    | no             | Attribute shown as email in the preview; never used to match accounts                                                                                                                                               |
| `sp_certificate_pem` | server-managed | The SP signing certificate. Kuvryn Ant generates a 2048-bit RSA key and a self-signed certificate valid for two years when the provider is created; set `rotate_sp_certificate: true` on an update to regenerate it |

### Signing keys

For stored providers the SP key pair is created and stored (encrypted) by Kuvryn Ant; you never upload a private key. After `rotate_sp_certificate`, re-import the SP metadata at the IdP.

IdP signing certificates come from the metadata. Several certificates may be present for a rollover overlap; to retire one, replace the metadata. Expired certificates are rejected when you save, and the console and `certificates` array show expiry (`ok`, `expiring`, `expired`, `not_yet_valid`).

### Workflow

1. **Read the IdP metadata.** `POST /api/v1/identity-providers/saml/import-metadata` with `{"xml":"<metadata>"}` parses the pasted XML and reports the IdP entity ID, SSO and SLO endpoints, certificates with expiry, `valid_until`, errors and warnings. It changes nothing.

   ```sh
   export ANT=https://ant.example.com
   H="Authorization: Bearer $ANT_TOKEN"; J='Content-Type: application/json'
   jq -Rs '{xml: .}' idp-metadata.xml \
     | curl -fsS -X POST $ANT/api/v1/identity-providers/saml/import-metadata -H "$H" -H "$J" -d @- | jq
   ```

2. **Create** the provider disabled:

   ```sh
   jq -n --rawfile xml idp-metadata.xml --arg role "$ROLE_ID" '{
     kind:"saml", name:"Corporate SSO", enabled:false,
     config:{entity_id:"https://ant.example.com/auth/saml/metadata",
             idp_entity_id:"https://idp.example.com/saml",
             idp_metadata_xml:$xml,
             roles_attribute:"groups",
             claim_roles:[{value:"ant-readers", role_ids:[$role]}]}}' \
   | curl -fsS -X POST $ANT/api/v1/identity-providers -H "$H" -H "$J" \
       -H 'Idempotency-Key: idp-saml-1' -d @- | jq '{id, revision}'
   ```

3. **Give the IdP the SP metadata:** `GET /api/v1/identity-providers/{id}/metadata` (Download SP metadata in the console), then import it at the IdP and check the ACS and SLO URLs above.
4. **Preview** with a SAML response copied from the browser developer tools (`POST /api/v1/identity-providers/{id}/preview` with `{"sample":"<base64 or XML>"}`). The preview does not verify the signature; the real sign-in does.
5. **Enable** (`POST /api/v1/identity-providers/{id}/enable` with `If-Match: "<revision>"`). The provider is built first; a broken configuration fails with 422. Enabling replaces any other enabled SAML provider, without a restart.

Role mapping follows the same rules as OIDC: explicit bindings win, unmapped identities without a binding are refused, claim-managed memberships are reconciled at every sign-in. See [OIDC role mapping](./oidc.md#role-mapping).

## Option 2: mounted file (`ANT_SAML_CONFIG_FILE`)

Strict JSON (only these keys, no duplicates or `null`, at most 64 KiB), read at startup, shown read-only, and it takes precedence over a stored SAML provider.

| Key                | Meaning                                                                    |
| ------------------ | -------------------------------------------------------------------------- |
| `origin`           | `https://<host>`; must match `ANT_PUBLIC_URL` and the other provider files |
| `entity_id`        | SP entity ID                                                               |
| `idp_entity_id`    | IdP entity ID                                                              |
| `metadata_file`    | Path to the IdP metadata XML                                               |
| `key_file`         | Path to the SP private key (PEM, RSA, at least 2048 bits)                  |
| `certificate_file` | Path to the SP certificate (PEM) matching the key                          |

The ACS, metadata and logout URLs are derived from `origin` (`/auth/saml/callback`, `/auth/saml/metadata`, `/auth/saml/slo`).

```json
{
  "origin": "https://ant.example.com",
  "entity_id": "https://ant.example.com/auth/saml/metadata",
  "idp_entity_id": "https://idp.example.com/saml",
  "metadata_file": "/run/ant/saml/idp-metadata.xml",
  "key_file": "/run/ant/saml/sp.key",
  "certificate_file": "/run/ant/saml/sp.crt"
}
```

Generate an SP key pair, for example:

```sh
openssl req -x509 -newkey rsa:3072 -nodes -days 730 \
  -keyout sp.key -out sp.crt -subj "/CN=ant.example.com"
kubectl -n ant create secret generic ant-saml \
  --from-file=config.json=./saml.json --from-file=idp-metadata.xml \
  --from-file=sp.key --from-file=sp.crt
```

```yaml
auth:
  saml:
    existingSecret: ant-saml
    configKey: config.json
```

The chart mounts the Secret at `/run/ant/saml/` and sets `ANT_SAML_CONFIG_FILE`.

::: warning File providers have no role mapping
The file has no `roles_attribute` or `claim_roles`. Users sign in only through explicit identity bindings: `POST /api/v1/identity-bindings` with `issuer` set to the IdP entity ID and `subject` set to the user's NameID. See [Explicit bindings](./oidc.md#explicit-bindings).
:::

## Logout

`POST /auth/logout` (the console's sign-out) always ends the local session first. A failed or missing IdP leg never restores it.

**SP-initiated single logout.** If the session came from SAML and the IdP metadata publishes an HTTP-POST `SingleLogoutService`, the server stages a signed `LogoutRequest` (NameID, its format and the `SessionIndex` recorded at sign-in), bound to the browser. The console navigates to `/auth/saml/logout`, which posts it to the IdP. The IdP's signed `LogoutResponse` returns to `/auth/saml/slo` and is checked against the single-use state, signature, issuer, destination and replay. Audit: `auth.saml_logout.success`.

**IdP-initiated logout.** A `LogoutRequest` posted to `/auth/saml/slo` is accepted only if it is signed by a certificate from the imported IdP metadata, issued by the configured IdP, addressed to this endpoint, recent, and not seen before. It ends all sessions of that NameID, or those with the given `SessionIndex` plus sessions with no recorded index, and is answered with a signed success `LogoutResponse` (ending zero sessions is still success). Unsigned or wrongly signed requests get HTTP 401 and end nothing. Audit: `auth.saml_logout.requested_by_idp`.

::: warning Limits
HTTP-POST binding only for logout (no Redirect or Artifact binding), unencrypted NameIDs, one `SessionIndex` per request. If you upgraded from a version without single logout, re-import the SP metadata at the IdP so it learns the `/auth/saml/slo` endpoint.
:::

## Troubleshooting

| Symptom                                                      | Likely cause                                                                                                                 |
| ------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------- |
| 401 "browser authentication failed" after the IdP posts back | Assertion not signed (only the response is), wrong audience, clock skew, replayed response, or no binding and no mapped role |
| Sign-in starts at the IdP portal and fails                   | IdP-initiated (unsolicited) sign-in is not supported; start from the console button                                          |
| Save rejected                                                | Expired IdP certificate in the metadata, `idp_entity_id` not matching the metadata, or invalid metadata XML                  |
| Logout does not end the IdP session                          | The IdP metadata has no HTTP-POST `SingleLogoutService`, or the IdP was not given the SLO URL                                |

::: warning Not validated against a live IdP in the repository tests
The SAML implementation is tested with locally signed fixtures, not against Keycloak, Entra ID, Okta or ADFS. Run and record a real sign-in and logout test per IdP before production use.
:::
