# Management API

Everything the console does goes through the management API at `/api/v1`. It is described by an OpenAPI 3 contract, [`api/openapi.yaml`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/api/openapi.yaml) (the file holds JSON content despite its extension). The server's request and response types are generated from that contract, and so is the console's client, so the contract is the authoritative reference. The MCP endpoint (`/mcp`) calls the same handlers in process, so authorization and audit are identical (see [MCP tools](./mcp.md)).

This page explains the conventions shared by all endpoints and shows a few worked examples. The complete, interactive reference is embedded at the end.

<a href="/Kuvryn-Ant/api/" target="_blank">Open the interactive API reference</a>

## Authentication

Every endpoint, except `GET /api/v1/status` for anonymous callers (name and version only) and the GitHub OIDC token exchange, requires a bearer token:

```http
Authorization: Bearer <TOKEN>
```

Tokens are service-account tokens or personal access tokens (see [Service accounts and tokens](../identity/tokens.md)); browser sessions use cookies with CSRF protection instead. A token's effective permissions are the **intersection** of its scopes and the principal's persisted role grants, and an explicit deny always wins (see [Roles and grants](../identity/roles.md)). Requests the caller is not allowed to make return `403`; resources the caller cannot read are generally reported as `404`.

GitHub Actions workflows can exchange their OIDC ID token for a short-lived scoped token with `POST /api/v1/auth/github-oidc/exchange` (see [GitHub OIDC trust](../identity/github-oidc.md)).

## Conventions

### Structured errors

Every error has the same JSON body and a matching HTTP status:

```json
{
  "code": "precondition_failed",
  "message": "resource revision changed",
  "request_id": "01JAB3Q5W6Z8K9M2N4P7R0S1T3"
}
```

`code` is a stable machine-readable value: `invalid_request`, `unauthenticated`, `forbidden`, `not_found`, `conflict`, `precondition_failed`, `unavailable`, `internal_error`, `rate_limited`, plus federation-specific codes (`federation_not_configured`, `not_authority`, `member_unreachable`, `invitation_invalid`, `stale_generation`, `document_rejected`, `federation_managed`, `not_federated`, `feed_home_elsewhere`, `feed_transferring`). `request_id` is also returned in the `X-Request-ID` header of every response and appears in the server log, so a failing call can be traced to its log line.

Unknown JSON fields are rejected (`400 invalid_request`).

### Cursor pagination

List endpoints are paginated with opaque, server-signed cursors:

```sh
curl -fsS "$ANT/api/v1/feeds?limit=50" -H "$H"
# {"items":[...], "page":{"has_more":true,"next_cursor":"..."}}
curl -fsS "$ANT/api/v1/feeds?limit=50&cursor=<next_cursor>" -H "$H"
```

`limit` is 1 to 200 (default 50). Some operational endpoints (retention, prewarm, replication history) page by an `after` ID instead and return `next_after`; the contract states which form each endpoint uses. Cursors are authenticated with the root key, so they cannot be forged or edited.

### Revisions, ETag and If-Match

Mutable resources carry a positive integer `revision`. A `GET` returns it in the body and as a quoted `ETag` header (`ETag: "3"`). Every update and delete requires `If-Match` with the quoted current revision:

| Situation                          | Response                               |
| ---------------------------------- | -------------------------------------- |
| `If-Match` missing                 | `428` (`precondition_failed`)          |
| Revision changed since you read it | `412` (`precondition_failed`)          |
| Success                            | New revision in the body and in `ETag` |

Read, modify, write with the ETag you read; on `412`, read again and reapply your change.

### Idempotency-Key

Mutations (`POST`, `PUT`, `PATCH`, `DELETE`) require an `Idempotency-Key` header of 1 to 256 bytes; a missing key is `400 invalid_request`. The server stores the outcome for 24 hours, keyed by principal, operation and resource:

- Retrying the same request with the same key returns the stored result (with the original status and `ETag`) without applying it again. This makes retries after a timeout safe.
- Reusing a key with a **different** request body returns `409 conflict` (`idempotency key conflict`).

Use a fresh key per logical change (a UUID, or a name such as `npm-feed-1` in scripts you re-run deliberately).

Exceptions, as stated per operation in the contract: issuing a service token (`POST /api/v1/principals/{id}/tokens`) **rejects** an `Idempotency-Key`, because its secret is returned once and never stored for replay. Read-only `POST` operations (tests, previews, `:test` endpoints, the transfer and migration endpoints, the GitHub OIDC exchange) and some federation operations do not take one.

### Audit in the same transaction

Every mutation writes its audit event (and, where applicable, its webhook outbox event) in the same database transaction as the change. A committed change always has its audit record; a failed one never does. Query the log with `GET /api/v1/audit` or export it with `GET /api/v1/audit/export`, and stream it to a SIEM with the [audit sink](../operations/observability.md#audit-sink-siem-streaming).

### Rate limits and capacity

When a configured [rate limit](../operations/storage-maintenance.md#quotas-and-rate-limits) applies, the API answers `429 rate_limited` with `Retry-After`. When the server's admission limit (`ANT_MAX_REQUESTS`) is exhausted it answers `503 unavailable` with `Retry-After: 1`.

## Worked examples

The examples use these shell variables:

```sh
export ANT=https://ant.example.com
H="Authorization: Bearer $ANT_TOKEN"
J='Content-Type: application/json'
```

### Check status

```sh
curl -fsS $ANT/api/v1/status -H "$H" | jq
```

With an instance-admin token the response includes database, blob storage and writer-lease readiness and any `warnings`.

### Create an npm feed with a local and a remote source

```sh
# 1. Local source (publish target)
SRC=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-local-1' \
  -d '{"name":"npm-local","kind":"local","format":"npm"}' | jq -r .id)

# 2. Remote source for the public registry
REM=$(curl -fsS -X POST $ANT/api/v1/sources -H "$H" -H "$J" -H 'Idempotency-Key: npm-remote-1' \
  -d '{"name":"npmjs","kind":"remote","format":"npm"}' | jq -r .id)

# 3. Its upstream configuration (needs the source revision)
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/sources/$REM -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PUT $ANT/api/v1/sources/$REM/remote-config -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: npm-remote-cfg-1' -d '{
  "origin":"https://registry.npmjs.org","redirect_origins":[],"allow_private":false,
  "timeout_seconds":60,
  "cache":{"metadata_ttl_seconds":300,"negative_ttl_seconds":60,"stale_if_error_seconds":3600,
           "max_entries":10000,"max_bytes":268435456}}'

# 4. Feed with both sources in read order and a publish policy, in one transaction
curl -fsS -X POST $ANT/api/v1/feeds/batch -H "$H" -H "$J" -H 'Idempotency-Key: npm-feed-1' -d @- <<JSON
{"slug":"npm","format":"npm","source_ids":["$SRC","$REM"],
 "publish_policy":{"default_source_id":"$SRC","rules":[]}}
JSON
```

Clients then use `https://ant.example.com/feeds/npm/npm/` (see [npm](../formats/npm.md)).

### Update a feed with If-Match

```sh
FEED=$(curl -fsS "$ANT/api/v1/feeds?prefix=npm" -H "$H" | jq -r '.items[] | select(.slug=="npm") | .id')
REV=$(curl -fsS -D- -o /dev/null $ANT/api/v1/feeds/$FEED -H "$H" | tr -d '\r' | awk 'tolower($1)=="etag:"{print $2}')
curl -fsS -X PATCH $ANT/api/v1/feeds/$FEED -H "$H" -H "$J" -H "If-Match: $REV" \
  -H 'Idempotency-Key: npm-feed-anon-1' -d '{"anonymous_read":true}'
```

### Explain how a package resolves

```sh
curl -fsS "$ANT/api/v1/feeds/$FEED/resolve?coordinate=lodash" -H "$H" | jq
```

The response lists the feed's sources in read order and which one would serve the coordinate, and why (see [Read order and resolve explain](../concepts/resolution.md)).

### Issue a scoped service token

```sh
curl -fsS -X POST $ANT/api/v1/principals/<PRINCIPAL_ID>/tokens -H "$H" -H "$J" -d '{
  "scopes":[{"feed_id":"'"$FEED"'","actions":["read","publish"]}],
  "expires_at":"2026-12-31T23:59:59Z"}'
```

The token secret is returned once, in this response only; this is the one mutation that rejects an `Idempotency-Key`. Expiry must be in the future and within one year. Scope actions are `read`, `publish`, `delete`, `configure`, `replicate` and `admin`, optionally limited to a feed and a namespace. List token metadata with `GET /api/v1/tokens` and revoke with `DELETE /api/v1/tokens/{id}`.

## Interactive reference

The reference below is rendered from the same OpenAPI contract the server is generated from. It opens in its own page as well: <a href="/Kuvryn-Ant/api/" target="_blank">Open the interactive API reference</a>. The raw contract is [api/openapi.yaml on GitHub](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/api/openapi.yaml).

<iframe src="/Kuvryn-Ant/api/" style="width:100%;height:80vh;border:1px solid var(--vp-c-divider);border-radius:8px"></iframe>
