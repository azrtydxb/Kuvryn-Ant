# Identity: sign-in providers, role mapping, single logout and real-provider validation

Browser sign-in uses OIDC, SAML 2.0 or LDAP/Active Directory, configured under
Access, Sign-in providers (`/api/v1/identity-providers`) or, read-only, from the
`ANT_OIDC_CONFIG_FILE`, `ANT_SAML_CONFIG_FILE` and `ANT_LDAP_CONFIG_FILE` files.
The break-glass account (`break-glass.md`) is independent of all of them.

Service accounts and API tokens are not covered here.

## Routes

All browser routes live under `/auth/`, which the chart already routes
(`deploy/chart/templates/httproute.yaml` and `ingress.yaml` list `/auth`).

| Route                                              | Purpose                                                                                                |
| -------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| `GET /auth/oidc/login`, `GET /auth/oidc/callback`  | OIDC authorization code flow with PKCE, `state` and `nonce`                                            |
| `GET /auth/saml/login`, `POST /auth/saml/callback` | SAML SP-initiated sign-in (HTTP-Redirect out, HTTP-POST back)                                          |
| `GET /auth/saml/metadata`                          | SP metadata (entity ID, ACS, single logout endpoint)                                                   |
| `POST /auth/logout`                                | Ends the local session; for a SAML session also starts single logout                                   |
| `GET /auth/saml/logout`                            | Serves the staged, signed `LogoutRequest` once, as a self-submitting form to the IdP                   |
| `POST /auth/saml/slo`                              | The SP single logout endpoint: receives the IdP's `LogoutResponse` or an IdP-initiated `LogoutRequest` |
| `GET/POST /auth/ldap/login`                        | LDAP sign-in (pre-login CSRF state, then credentials)                                                  |
| `POST /auth/local/login`                           | Local accounts and break-glass                                                                         |

## Role mapping

Authorization never comes from a provider directly. A provider can only propose
existing **local** roles through a mapping, and the proposal applies at login:

| Provider  | Source of values                                                         | Mapping fields                   |
| --------- | ------------------------------------------------------------------------ | -------------------------------- |
| OIDC      | ID-token claim (array, string, or object keys, as Zitadel project roles) | `roles_claim`, `claim_roles`     |
| SAML      | Assertion attribute                                                      | `roles_attribute`, `claim_roles` |
| LDAP / AD | Immutable group IDs (base64url `entryUUID` / `objectGUID`)               | `group_roles`, `group_nesting`   |

Rules, identical for all three:

- **Explicit bindings win.** An identity with an explicit binding (created by an
  administrator) and its memberships is never touched by a mapping.
- **Just-in-time creation.** An identity with no binding and at least one mapped
  role gets a user principal and a claim-managed binding on first login. An
  identity that maps to no role and has no binding is refused, and nothing is
  created. An existing principal with that issuer and subject is never taken over.
- **Reconciliation.** For claim-managed identities the claim-origin memberships
  are reconciled to exactly the mapped roles at every login, so a role removed at
  the provider is dropped at the next login. Sessions already issued keep their
  roles until they expire (at most the session lifetime, 8 hours by default).
- **Only local roles** can be referenced; the API rejects unknown or
  federation-managed role IDs (HTTP 400).
- **Fail closed.** If a configured mapping cannot be applied, no session is issued.
- A provider with no mapping configured never grants anything from groups or
  roles.

LDAP group nesting is per provider (`group_nesting`):

- `nested` (default): groups that are members of groups are followed up to the
  directory depth limit (10) and size limits. A role mapped to an outer group is
  inherited by members of inner groups. A cycle, depth or size overflow refuses
  the sign-in.
- `direct`: only groups the user is a direct member of. Use it when roles must
  not leak through group nesting.

Audit: `browser.claim_user_provisioned` and `browser.claim_roles_applied`
(principal IDs only, never issuer, subject, claim or group values), plus the
sign-in events `auth.<oidc|saml|ldap>.success` and `.failure`. The sign-in
preview (`POST /api/v1/identity-providers/{id}/preview`) shows the roles a
mapping would propose for a pasted token, assertion or LDAP login.

## SAML single logout

SP-initiated: the console's sign-out calls `POST /auth/logout`. The local session
always ends first. If the session came from the SAML provider and the IdP
metadata publishes an HTTP-POST `SingleLogoutService`, the server stages a signed
`LogoutRequest` (with the NameID, its format and the `SessionIndex` recorded at
sign-in) bound to the browser, and the console navigates to `/auth/saml/logout`,
which posts it to the IdP. The IdP's signed `LogoutResponse` returns to
`/auth/saml/slo` and is checked against the staged single-use state, the signature,
the issuer, the destination, and replay. A failed or missing IdP leg never
restores the local session.

IdP-initiated: a signed `LogoutRequest` posted to `/auth/saml/slo` is accepted
only if it is signed by a certificate in the imported IdP metadata, issued by the
configured IdP, addressed to this endpoint, recent, and not seen before. It ends
the matching sessions (all sessions of the NameID, or those with the given
`SessionIndex` plus any session with no recorded index) and is answered with a
signed success `LogoutResponse`. Ending zero sessions is still success.

Limits: HTTP-POST binding only (no Redirect or Artifact), unencrypted NameIDs,
one `SessionIndex` per request. Re-import the SP metadata at the IdP after
upgrading so it learns the single logout endpoint. The routes exist only while a
SAML provider is active.

## Profile menu, password change and sign-out

The console header has an account menu (the avatar: the OIDC `picture` claim, otherwise initials). It shows the display name, email or subject, the sign-in method and the roles, and links to My profile, personal access tokens, Change password, the theme and Sign out. `GET /api/v1/me` backs it; the profile page (`/console/` My profile) is the canonical place for personal access tokens (Settings, Tokens keeps the same list).

OIDC login now requests `openid profile email` (plus any configured extra scopes) so the name, email and picture are available. They are stored on the browser session only to display them, never to grant anything. The console CSP allows `img-src 'self' https:` for the picture.

### Change password

`/api/v1/me` returns `auth_method` (`local`, `oidc`, `saml`, `ldap`, `token`), `idp` (provider display name) and `password_change`:

| Session | `password_change.mode` |
| --- | --- |
| Local password user | `local`: the console opens a dialog and calls `POST /api/v1/me/password` |
| OIDC | `external`: the configured URL, else a known-product address derived from the issuer, else `unavailable` ("managed by <provider>") |
| SAML | `external` with the configured URL, else `unavailable` |
| LDAP | `external` with the configured URL, else `unavailable` ("managed by your directory") |
| API token | `unavailable`, and the console hides the item |

The URL is never taken from a request. Set it per provider with `password_change_url` (https only; the sign-in provider dialog in the console). Without it, OIDC providers are recognised from the operator's issuer and approved endpoints:

- Zitadel (`*.zitadel.cloud`, or `/oauth/v2/token` among the approved endpoints): `<issuer>/ui/console/users/me/password`, the Console route `me/password` for the signed-in user (current password, new password, confirmation).
- Keycloak (issuer path contains `/realms/`): `<issuer>/account/#/security/signingin`.
- Microsoft Entra (`login.microsoftonline.com`): `https://mysignins.microsoft.com/security-info`.
- Google: `https://myaccount.google.com/security`.
- Okta (`*.okta.com`, `*.oktapreview.com`): `https://<host>/enduser/settings`.

A self-hosted Zitadel whose Console is served from another path needs `password_change_url` set explicitly.

`POST /api/v1/me/password` (local users only) verifies the current password, applies the password policy (14 to 256 bytes), is limited to 10 attempts per minute per user, and counts wrong current passwords toward the same lockout as sign-in. On success it keeps the session making the change, ends the user's other browser sessions, and audits `auth.local.password_changed` (failures: `auth.local.password_change_failed`).

### Sign-out

`POST /auth/logout` ends the local session. SAML sessions continue to the IdP single logout. OIDC sessions also do RP-initiated logout: when the provider's discovery document advertises an `end_session_endpoint` on the issuer's origin (or among the approved endpoints), the response is `200 {"oidc_logout": "<url>"}` and the console navigates there with `id_token_hint`, `client_id` and `post_logout_redirect_uri=<origin>/console/`. Register `<origin>/console/` as a post-logout redirect URI in the IdP client. The ID token used for `id_token_hint` is kept encrypted (secrets keyring, bound to the instance and session row), decrypted only when the logout URL is built, and deleted with the session. Turn the redirect off per provider with "Also sign out at the provider" (`no_end_session`).

## Validating against real providers

Unit and integration tests use synthetic fixtures. They do not prove interop with
a real IdP, so validate each provider type once per environment and record the
result (date, provider version, build digest, outcome) in the release evidence.
Never record tokens, assertions, passwords or secrets.

Common preparation: set the public origin (`ANT_PUBLIC_URL`, https), create a
local role to receive mapped users (for example `sso-readers`) and note its UUID
(Access, Roles), and keep a second browser profile for the sign-in tests. Keep the
break-glass account usable throughout.

### OIDC against Zitadel (live)

1. In Zitadel create a project with the roles you want to map (for example
   `ant-readers`) and an application of type Web, authentication method PKCE or
   code with client secret. Redirect URI: `https://<origin>/auth/oidc/callback`.
   Enable "Assert roles on authentication" on the project and, if the claim should
   be there for the ID token, "Add user roles to the ID token" on the application.
   Grant a test user a role.
2. In Ant: Sign-in providers, Add provider, OIDC. Set the issuer to the Zitadel
   instance URL and the client ID and secret. Click Test connection, then "Use
   discovered endpoints" and run the test again until it passes (the endpoints
   must be approved explicitly).
3. Set Roles claim to `urn:zitadel:iam:org:project:<project id>:roles`, Extra
   scopes to `urn:zitadel:iam:org:projects:roles`, and the mapping to
   `ant-readers=<local role UUID>`. Save disabled, then use the preview with a
   pasted ID token to confirm the proposed role. Then enable.
4. Sign in with the test user in the second profile. Expect: console loads with the
   mapped role only; a `browser.claim_user_provisioned` audit event; no binding
   was created by hand.
5. Negative checks: a user without the Zitadel role is refused (HTTP 401, no
   principal); removing the role in Zitadel and signing in again drops it
   (`browser.claim_roles_applied`); an administrator-created explicit binding is
   not changed when the claim differs; replaying an old callback URL fails; tampering
   with `state` fails.
6. Record: Zitadel version, claim shape observed, sign-in latency, any clock skew
   warning from the test.

A second OIDC provider (Keycloak or Entra ID) should repeat steps 1 to 5 with its
own claim (`groups` array for Keycloak with a group mapper; `roles` or `groups`
for Entra ID app roles). Provider-specific claim shapes are the usual failure.

### SAML against a real IdP (Keycloak, Entra ID, Okta, ADFS)

1. Download the IdP metadata XML. In Ant: Add provider, SAML; paste the XML, click
   Read metadata (certificate expiry warnings show here), and set the SP entity ID
   (default `https://<origin>/auth/saml/metadata`). Save, then Download SP metadata
   and import it at the IdP. Confirm in it: ACS `https://<origin>/auth/saml/callback`
   (HTTP-POST) and, when single logout is wanted, `https://<origin>/auth/saml/slo`
   (HTTP-POST).
2. At the IdP: sign assertions (the assertion itself must be signed, a signed
   response alone is not enough), NameID format persistent (the default) and a
   stable value, audience equal to the SP entity ID, no assertion encryption (not
   supported), and an attribute for roles or groups (for example `groups`).
3. Set Roles attribute and the mapping, save disabled, paste a SAMLResponse from
   the browser dev tools into the preview, then enable. The preview does not
   verify the signature; the real sign-in does.
4. Sign in. Expect the mapped role, `auth.saml.success`, and a
   `browser.claim_user_provisioned` event for a new user.
5. Single logout, if the IdP supports it: sign out from the console and confirm the
   IdP session ends (re-opening the IdP shows the login page), with
   `auth.saml_logout.success`. Then sign in again and log out from the IdP's own
   portal; the Ant session must end (next API call is 401) and
   `auth.saml_logout.requested_by_idp` is recorded.
6. Negative checks: an assertion replayed through the browser back button fails;
   an expired certificate in the metadata is rejected at save; an unsigned or
   wrongly signed IdP logout request is rejected (HTTP 401) and ends nothing.
7. Record clock skew between the IdP and Ant: assertions are validated without
   skew tolerance.

### LDAP and Active Directory

1. Create a read-only service account that can bind and search the user and group
   bases. Use `ldaps://` (or `ldap://` with StartTLS). Give the directory's CA as
   the provider CA if it is private. Certificates are always verified.
2. Immutable IDs: OpenLDAP and 389 use `entryUUID`; Active Directory uses
   `objectGUID`. Set user and group ID attributes accordingly (login attribute
   `uid` or `sAMAccountName`; classes `inetOrgPerson`/`groupOfNames` or
   `user`/`group`; member attribute `member`). Group IDs in the mapping are the
   base64url (no padding) encoding of the raw ID bytes. The sign-in preview with a
   test login (no password) shows the resolved subject, group IDs and proposed
   roles, which is the easiest way to read the group IDs.
3. Save disabled, run Test connection (TLS, bind, read access to both bases), run
   the preview for a user in a nested group, then enable.
4. Sign in as: (a) a directly mapped user, (b) a user only in a nested inner group.
   With `group_nesting: nested`, (b) inherits the outer group's role; switch to
   `direct` and sign in again to confirm it does not. Expect
   `browser.claim_roles_applied` each time roles change.
5. Negative checks: wrong password gives one generic 401 with no directory text; a
   user in no mapped group and with no binding is refused; a group cycle or more
   than 10 levels refuses sign-in; a pre-login CSRF state cannot be reused; removing
   the user from the group drops the role at the next sign-in.
6. Record: directory product and version, the observed group-resolution depth,
   bind and search latency.

### What the tests cannot prove

The package tests cover protocol validation, mapping logic and routes against
fixtures; a real run is what shows IdP-specific behavior (claim shapes, NameID
formats, signing options, logout bindings, group schemas, clock skew, TLS
chains). Until such a run is recorded for a provider, treat it as unvalidated in
release notes.
