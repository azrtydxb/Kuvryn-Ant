# Administration console

React/TypeScript console for the existing `api/openapi.yaml` management contract.
No mock data, external fonts, analytics, provider provisioning, or backend authentication changes.

## Build and verify

Use Node 22.22 or newer and npm 10. From `web/`:

```sh
npm ci
npm run lint
npm test
npm run build
```

The committed lockfile pins dependency resolution. The build checks generated contract types, checks TypeScript, then emits hashed JS/CSS and `index.html` into `internal/console/dist/`. Keep those assets with the Go package so `go build` does not require Node. After an intentional OpenAPI change, run `npm run generate` and review the generated changes before building. The generator reads the repository's JSON-formatted OpenAPI document and fails on unsupported schema shapes.

From the repository root, `go test ./internal/console` checks embedded assets, routing, HEAD, security headers, and missing-file/directory behavior. This package has no database access, so it needs no PostgreSQL fixtures. Frontend tests use isolated fetch mocks; they do not establish real-backend integration evidence.

## Parent registration

No additional Go dependencies are required. Register both routes on the existing mux, without StripPrefix:

```go
mux.Handle("/console", console.Handler())
mux.Handle("/console/", console.Handler())
```

Import `github.com/azrtydxb/Kuvryn-Ant/internal/console`. Keep management APIs registered through their existing authentication and authorization middleware. The static shell is public and contains no credentials or protected data. Serve deployed traffic over HTTPS. The server adds a strict CSP with only same-origin scripts, styles, and fetches; no inline script/style or eval permission, no framing, no native form submissions. Missing assets and API routes return 404 instead of the SPA shell. HTML is not cached; hashed assets are immutable.

For development, run the real service on localhost:8080 and `npm run dev` (localhost:5173/console/). Vite forwards `/api` and `/auth` to that service without replacing its authentication or rewriting Origin. Browser sessions require HTTPS and the backend configured public origin to match the console; use the Go-served console for the parent authentication fixture. Never use the development server for deployment.

`go run web/scripts/serve.go` is a production static-only preview on localhost:5173. It intentionally has no API: Overview shows HTTP 404. It is useful for checking the embedded build, CSP, keyboard navigation, and responsive layout without pretending a backend exists.

## Behavior and limits

- Overview displays the real status endpoint and last successful check. Reachability is not database/storage readiness. Operational metrics and deployed protocol capabilities are not present in this contract, and the UI says so.
- Feed and source lists are permission-filtered and paginated, with prefix filters. Feeds can be created/edited; sources can be created. Source kind and format are configuration only. Remote sources must be configured (HTTPS origin, redirect allowlist, optional operator CA ID, timeout and cache bounds) before upstream credentials can be attached; credentials stay encrypted per source and are never returned.
- Feed attachments use explicit priorities (lower first) and the feed's ETag. Set an unoccupied priority to move a source; there is no atomic swap endpoint. Hidden sources may occupy priorities, so server conflicts remain authoritative.
- Publish policy is a full replacement with structured selectors, deny rules, default destination, and local target UUIDs. Each destination is shown before saving. The server validates attachments, overlap, and permissions. There is no routing-explanation endpoint, so the console does not fabricate a resolution preview.
- Mutations whose OpenAPI parameters require replay protection send idempotency keys. Token issuance explicitly rejects a replay key and is treated as a one-shot operation. An unchanged retry after a network/server/unreadable-response failure reuses the key within the current session. Feed/source/policy edits send the exact returned ETag in If-Match. Principal memberships and role grants quote the revision returned in their collection records, as the identity API has no individual GET endpoint. HTTP 412 asks the operator to refresh; drafts are not silently retried over newer configuration. Refresh discards unsaved drafts.
- The API does not expose effective-grant discovery. The client does not infer configuration authority from read access. Protected operations always go to the existing server and show explicit 401/403 errors.
- Browser sessions are discovered automatically with `GET /auth/session`, using same-origin HttpOnly cookies. CSRF values stay in memory and are sent as `X-CSRF-Token` on unsafe API requests. The client never reads cookies, manually supplies Origin, or persists credentials in local/session storage, URLs, or logs. No raw HTML is rendered.
- `GET /auth/providers` returns `{ "providers": ["oidc", "saml", "ldap"] }`; only configured providers are shown as sign-in actions. Unknown/unavailable discovery is shown with a retry, without inventing provider availability. OIDC/SAML links navigate to `/auth/oidc/login` and `/auth/saml/login`; trusted callbacks redirect to `/console/`.
- LDAP clears the password input immediately on submit, gets a fresh one-use challenge from `GET /auth/ldap/login`, and sends JSON `{username,password}` to that same route with `X-CSRF-Token`. Browser-generated Origin and cookies remain intact. After success the client re-reads `/auth/session`.
- Browser sign-out clears protected UI and in-memory credentials, POSTs `/auth/logout` with CSRF, and displays an explicit retry if server revocation cannot be confirmed. A retry re-fetches the authoritative session before attempting revocation. Local clearing is not reported as confirmed remote logout on an HTTP error.
- Service-token fallback remains in a collapsed development/admin form, memory-only, with cookies omitted from API requests. It clears on sign-out, leaving the page, or after 15 minutes; this does not revoke the service token itself. Browser-session expiry instead follows the server-provided expiry, with idle/disabled/revoked checks enforced server-side.
- Identity administration lists/creates users and service principals, lists/creates roles, replaces memberships and structured grants, and creates exact issuer/immutable-subject bindings to explicit enabled users. UUID keyset pagination uses `next_after`. Existing bindings cannot be listed or deleted with this contract; no such controls are shown.
- Token issuance is available only for enabled service principals. It uses explicit actions/scopes and future expiry within one year. The secret is displayed once in a password field, selectable for copying, and cleared after one minute or leaving the view. Issuance is not automatically retried; a failed/lost response requires explicitly starting a separate issuance. Token revocation accepts a recorded token ID with confirmation because the API has no token-list operation.

HTTP revision behavior follows [RFC 9110 If-Match](https://www.rfc-editor.org/rfc/rfc9110.html#name-if-match). Static policy directives were checked against [W3C CSP Level 3](https://www.w3.org/TR/CSP3/).

## Integration handoff (2026-10-18)

The parent mounts `/console` and `/console/`; no additional Go modules are needed. Session responses currently use the Go field names `session.PrincipalID`, `session.InstanceID`, `session.ExpiresAt`, with top-level `csrf_token`. The console validates that envelope and never reads the HttpOnly cookies directly. The API client generation now records whether each operation declares an Idempotency-Key; this is essential for the non-replayable service-token endpoint.

New administration screens use the existing principal/role/token/binding contract. Webhook administration (create/update/rotate/disable and delivery history) and the package discovery view are included; both use generated operations from the current OpenAPI. Unused generated operations are not advertised as UI capabilities.

Run `python3 web/scripts/qa-fixture.py start` from the repository root to build the real app, initialize a private socket-only PostgreSQL cluster, bootstrap a token into a mode-0600 credential file, and start a loopback app. It prints only the origin, private credential filename, and process ID. Use T3 preview to enter that token without echoing it, then exercise normal UI forms. `python3 web/scripts/qa-fixture.py stop` stops the app/PostgreSQL and removes all private fixture data. The state pointer `web/.qa-fixture.json` is ignored. No fake API is provided by this fixture.

### Real backend QA evidence

On 2026-10-18, the production embedded build was exercised in T3 preview against the actual Go app and a disposable socket-only PostgreSQL database. No mock backend or external provider was used.

- Desktop 1440×1000 and mobile 390×844: loaded console with strict CSP; mobile document width remained 390px.
- Provider discovery returned an empty configured list, shown accurately; memory-only bootstrap token login succeeded.
- Created OCI source and feed (201), attached source at priority 10 (200), saved local default publish target (200), and edited feed slug (200).
- Created role and user (201), replaced role grants and user membership with revision guards (200), and created explicit issuer/subject identity binding (201).
- Issued a read-scoped service token (201), verified identity reads were denied (403), then revoked that token through the UI (200). No replay key was sent for issuance.
- Duplicate role name produced a visible 409. A concurrent feed update produced a visible 412 on stale UI save; the newer record was not overwritten.
- Read-back checks confirmed persisted feed/source/policy and role/membership state; PostgreSQL confirmed token revocation. The bootstrap secret was absent from app logs. Browser localStorage/sessionStorage stayed empty, URLs contained no tokens, and sign-out cleared password/protected views.
- All temporary app/PostgreSQL processes and private fixture data were stopped/removed after QA.

Live OIDC/SAML/LDAP providers were not configured in this fixture. Their browser-session transport, CSRF, LDAP prelogin/password clearing, and logout retry behavior are covered by frontend tests, not claimed as end-to-end provider authentication evidence. No remaining console/backend integration failure was observed in the exercised flows. The temporary shared keyfile compile blocker was resolved by its owner before QA.

### Run 3: packages, remote sources, webhooks (2026-10-18)

Took over the run-3 increment after the delegated console agent stalled. First fixed the three failing frontend tests (abort/session-end timing in `web/src/api.test.ts`, and the `Packages` mock fallback crashing two `App.test.tsx` flows), then re-verified against a fresh disposable socket-only PostgreSQL + real Go app served from this checkout (loopback, bootstrapped token, local image push over the OCI registry).

- Full web suite green again: 24/24 tests, `npm run build` (contract check + tsc + vite), prettier clean.
- Exercise pass in ego-browser against the real backend: created an OCI feed (201) and local source (201), attached the source at priority 10 with the feed's ETag (200, revision bumped), saved a structured publish policy default (200).
- Published a minimal OCI config blob + manifest through `/v2/packages-oci/team/app` (201). The new package discovery view listed `team/app` with its namespace and owning source; prefix filter (`team/`) still matched, exact-namespace filter (`team`) matched, and a wrong SHA-256 checksum returned an empty list.
- Created a remote OCI source (201), saved a full remote-config (HTTPS origin, cache bounds, private-flag default off) through the remote-config form (revision 3), then saved an upstream `Bearer` credential. The config read-back returned only the opaque credential reference (`credential_id`), never the secret, and the console rendered a detach toggle instead of the value.
- Webhook administration: created a webhook (201) with endpoint + cleared signing secret, revision 1 shown, and opened the delivery-history view.
- Identity: tokens tab rendered the revoke view; token issuance/revocation flows were already covered in run 2.

No regressions or remaining console/backend integration failures observed in the exercised run-3 flows.

## Console design and optional endpoints

The look comes from the vendored Azrty design system in `src/ds/` (tokens, components.css, self-hosted fonts and icon font, and `bundle.js` with a few local accessibility patches). Every screen uses generated operations. A server older than this console may lack some; a GET that answers 404, 405 or 501 renders "Not available on this server yet" instead of an error (`src/gaps.ts`). Admin-only areas (Access, Webhooks, Settings) are hidden when `GET /permissions/effective` says the browser session's principal lacks admin. Settings, About has a "Show backend notes" switch.

