# SAML implementation handoff (in progress)

Parent: install `github.com/crewjam/saml v0.5.1` (reviewed upstream SAML/XML signature implementation; no custom signature implementation). Package tests also directly use its dependencies `github.com/beevik/etree` and `github.com/russellhaering/goxmldsig`; use the versions selected by crewjam's module graph. This agent does not edit go.mod/go.sum.

Scope: internal/samllogin only; no migrations or OIDC edits. ReplayStore will require atomic persistent claim semantics in the parent's PostgreSQL integration. Browser session/CSRF and provider administration remain parent integration responsibilities.

## Integration API (available)

`New(Config) (*Provider, error)`, `Begin(relay string) (*url.URL, Pending, error)`, `Consume(ctx, rawXML, Pending) (Identity, error)`, `Metadata() ([]byte,error)`, `BeginLogout(Identity, relay) ([]byte, Pending,error)`, `ConsumeLogout(ctx,rawXML,Pending) error`.

`ReplayStore.Claim(context.Context, []string, time.Time) (bool,error)` must atomically insert ALL keys or none, return false if ANY live key exists, and persist across restarts until expiry. Login claims request + assertion keys; logout claims request + response keys. Keys contain `saml:<base64url IdP entity ID>:<kind>:<opaque ID>`; store constructor must additionally scope instance/provider. Supplied expiry is the later of assertion acceptance expiry and pending request expiry; logout uses pending expiry. Maximum pending lifetime is five minutes. Reject past expiry in the store and fail closed on SQL errors. Provider IDs and request/assertion IDs are untrusted-length bounded by the one-MiB XML limit; hash keys for bounded indexed storage if needed. Parent assigned postgres.go/postgres_test.go and migration012 to mutation_safety; provider agent will not edit them.

HTTP integration: enforce request body limit before decoding base64; pass decoded XML (max 1 MiB). Keep Pending in browser-bound server-side state, validate RelayState and CSRF/cookie binding on callback, and never construct Pending from callback XML. Map `(configured provider, Identity.Subject)` explicitly to local identities; email and groups convey evidence only, never administrator grants. Invalidate local session on logout regardless of remote success.

## Completed provider implementation

- New files: `internal/samllogin/saml.go`, `logout.go`, `saml_test.go`, `logout_test.go`. PostgreSQL files in this directory belong to mutation_safety.
- Trusted metadata bytes only; exact configured IdP entity, HTTPS endpoints, RSA >=2048 SP key matching certificate, active certificate checks, multiple trusted signing certificates for overlap and explicit metadata replacement for retirement. No metadata fetch/network access. Metadata export delegates to crewjam.
- Signed Redirect AuthnRequests, POST response consumption, strict exact audience/recipient/issuer/request correlation, explicit time checks with no clock-skew grace, single signed assertion/subject confirmation/authentication statement. NameID defaults to persistent; configure NameIDFormat explicitly for other formats.
- Structural XML size/depth/namespace/duplicate-ID/wrapping checks before the reviewed library. Require a valid assertion signature even when a valid signed response envelope exists; verification uses goxmldsig and crewjam, never custom cryptography. Signature-validation error details containing raw assertions are not exposed.
- Atomic persistent replay boundary, including request reuse and assertion reuse across requests; retain claims until the later of Conditions.NotOnOrAfter and Pending.Expires. Assertion validity cannot extend more than 24 hours into the future, opaque IDs are bounded to 1024 bytes, configured entity IDs to 2048 bytes. Update to earlier expiry note: retention intentionally covers the full conditions window even if subject confirmation/session expiry is shorter.
- Signed POST logout requests include exact consumed NameID format and session index. Signed POST LogoutResponses require exact issuer/destination/request, valid time and one-time replay claim. Parent must invalidate the local session independently.

## Verification and limits

Local fixtures are constructed using crewjam SAML schema and goxmldsig signing APIs with fresh ephemeral RSA certificates; no external provider fleet or private fixture credentials. Tests cover success/mapping, tampered/untrusted/unsigned signatures, signed-envelope substitution, wrapping/duplicate assertions, malformed XML/DTD/oversize, expired/future/missing claims, wrong audience/recipient/request/issuer, replay including concurrency, store failure, rollover/retirement, metadata export and signed logout/correlation/replay. No static upstream fixture files were copied.

This is a narrow provider implementation, not an enterprise-readiness claim. It requires one plaintext signed assertion, one bearer confirmation, one authentication statement and one audience restriction; encrypted assertions, unsolicited login/logout requests, Redirect logout responses, artifact binding and dynamic metadata are unsupported. Rollover certificates must already be valid at import. Strict time checks require synchronized clocks. Local unit fixtures are not a Keycloak interoperability fixture; the architecture's Keycloak acceptance run remains outstanding. HTTP routes, body decoding, browser state/cookies, CSRF, session issuance/revocation, configuration persistence/audit and explicit local principal resolution remain parent integration responsibilities. Exported attributes confer no permissions.

Dependency source review: https://github.com/crewjam/saml (v0.5.1), including service_provider.go signature/claim handling. The wrapper tightens the library's permissive missing-audience/multiple-assertion behavior. Library-backed tests do not constitute an independent security audit.

Final verification: `go test -race ./internal/samllogin` PASS; `go vet ./internal/samllogin` PASS. PostgreSQL integration tests supplied by the other agent skip without `ANT_TEST_DATABASE_URL`; this provider agent did not run a database fixture. No module edits, staging, commits, migrations, or changes to another agent's files were performed.
