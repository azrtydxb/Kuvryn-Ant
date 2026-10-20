# Webhooks and signing secrets

## Model

- A **signing secret** (`/api/v1/webhook-secrets`, console: Access, Signing secrets) is a named HMAC key: id, unique name, description, created/rotated times, revision and the webhooks using it. The value is encrypted at rest through `internal/secrets` (purpose `webhook`, key bound to instance and record id), is returned once by the create or rotate call that produced it, and is never readable again. An idempotent replay of create or rotate returns the metadata without the value.
- An **outgoing webhook** has a unique display **name** (a label, 1 to 100 printable characters, unique per instance ignoring case), the full target **URL**, one signing secret and an enabled switch. It is configured in the console only (Webhooks, Outgoing); nothing is read from deployment configuration.
- Rotating a secret affects every webhook that uses it. Deleting one is refused with 409 while any webhook references it.

## Target URL

The URL must be https (`ANT_WEBHOOK_ALLOW_HTTP=true` also permits http), without credentials, query or fragment. Outbound rules apply on configuration and again on every delivery: no localhost or private/special-use address literals, and DNS answers are checked on every delivery connection. Configuration never makes a network request.

`ANT_WEBHOOK_BASE_URL`, the `webhook_base_url` instance setting and `PUT /api/v1/settings/webhook-base-url` were removed. A set `ANT_WEBHOOK_BASE_URL` is ignored with a startup warning.

## Rotation and signatures

`POST /api/v1/webhook-secrets/{id}/rotate` takes an optional `value` (omitted: 32 random bytes are generated) and optional `grace_seconds` (0 to 604800). With a grace window the previous value is kept as a separate encrypted record and **signs deliveries together with the new one** until the window ends; the expired record is deleted by the next secret mutation.

Each delivery is `POST` with `X-Ant-Event-ID`, `X-Ant-Timestamp` and `X-Ant-Signature`. The signature is HMAC-SHA256 over `timestamp + "\n" + event id + "\n" + body`. `X-Ant-Signature` is a comma-separated list of `sha256=<hex>` entries: the current secret first and, only inside a grace window, the previous one. Receivers must accept the request when any entry matches a secret they hold; a receiver that compares the whole header to one value breaks during a grace window, so deploy list-aware verification before using grace.

## Upgrade

Migration 050 turned each inline webhook secret into a signing secret named `webhook-<first 8 characters of the webhook id>` that adopts the same ciphertext row, so signatures do not change. Migration 057 turns the old slug into the display name and drops the slug-versus-custom distinction: every existing webhook keeps its endpoint, signing secret and delivery history, webhooks that never had a name are labelled `webhook-<first 8 characters of the id>`, and the base URL setting column is dropped. The API stays compatible where cheap: `POST /api/v1/webhooks` still accepts `endpoint` plus `secret_id` (or the legacy inline `secret`) without a `name`, and a name is generated. The legacy `PUT /api/v1/webhooks/{id}/secret` still rotates the shared secret the webhook uses, without grace.

## Incoming webhooks

An incoming hook is served at `<ANT_PUBLIC_URL>/hook/<slug>` (and `/hook/<slug>/<token>` for the secret-in-URL mode) and runs one action. User documentation with per-provider setup lives in the site (Webhooks section); this is the operator view.

### Model (migration 058)

- `incoming_hooks`: slug (`[a-z0-9-]`, 1 to 63, unique per instance), description, enabled, `auth_mode` (`hmac`, `url_token`, `bearer`), `hmac_format`, `signing_secret_id` (a signing secret, so it cannot be deleted while used), `token_hash` and `token_hint` (the URL token is shown once and stored only as a SHA-256 digest), `bearer_scope`, `action_type` and the `action` JSON, `rate_per_minute`, creator, revision.
- `incoming_hook_deliveries`: the delivery log and the work queue in one table. Accepted calls store the payload (so they can be redelivered), a redacted 2 KiB preview, the allowlisted request headers, the source address and the authentication result; rejected calls keep only a short redacted preview. Workers claim rows with `FOR UPDATE SKIP LOCKED`, so processing is async and survives restarts (a crashed claim is retried after its two-minute lease).
- `incoming_hook_replay`: provider delivery ids, kept 24 hours.

Processing runs only under the instance writer lease, in the same worker group as webhook delivery and replication. The payload of an accepted call is retained for the retention window; it is visible only through the redacted preview, never through the API.

### Authentication

| Mode        | Credential                                                                                  | Checked                                                                                  |
| ----------- | ------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------- |
| `hmac`      | A signing secret with format `github`, `gitlab`, `ant`, `npm`, `artifactory` or `nexus`     | After reading the body (capped), constant time, current and grace-window previous secret |
| `url_token` | 256-bit random suffix `hk_...`                                                              | Before the body is read, SHA-256 digest compared in constant time                        |
| `bearer`    | `Authorization: Bearer ant_...` holding `configure`, `replicate` or `admin` on the instance | Before the body is read; token scopes and role grants both apply                         |

Every refusal before acceptance is `404` with one fixed body, whether the slug is unknown, the hook disabled or the credential wrong. `413` is answered on the declared length before anything else; `429` only to authenticated callers (and uniformly per client address); `409` for a repeated provider delivery id. `202` carries the delivery id.

### Actions and identity

Actions run as the hook's creator with that principal's current roles, so removing the creator's rights or disabling the principal stops them. `upstream_notify` drops metadata and negative-cache entries in the in-process remote runtime (`remoteruntime.Manager.InvalidateMetadata`); `prewarm` creates a one-off job `hook:<slug>:<delivery>` and queues its run; `replication_run` sets `run_requested` on the job; `purge_source_cache` runs the same transaction as `DELETE /api/v1/sources/{id}/cache`; `clean_cache` calls `artifact.CleanCache`; `relay` posts through `events.Sender`, the delivery transport of outgoing webhooks, with up to 8 attempts and the same backoff.

State-changing actions write audit events: `source.cache.purge` (as the API does) and `incoming_hook.prewarm`, `incoming_hook.replication_run`, `incoming_hook.upstream_notify`. Configuration changes write `incoming_hook.create`, `.patch`, `.token`, `.delete`, `.test` and `.redeliver`. As with every audit event they are also emitted to the outbox, so outgoing webhooks can subscribe to them.

### Configuration

| Variable                      | Default   | Meaning                                                                                       |
| ----------------------------- | --------- | --------------------------------------------------------------------------------------------- |
| `ANT_PUBLIC_URL`              | none      | Origin used to build hook URLs in the console                                                 |
| `ANT_HOOK_MAX_BODY_BYTES`     | `1048576` | Request body cap (1024 to 8388608)                                                            |
| `ANT_HOOK_LOG_RETENTION_DAYS` | `14`      | Delivery log retention (1 to 365)                                                             |
| `ANT_WEBHOOK_ALLOW_HTTP`      | `false`   | Permit http relay and webhook targets (host rules still apply)                                |
| `ANT_TRUSTED_PROXIES`         | empty     | Proxies whose forwarding headers decide the recorded source address and the per-address limit |

Rate limits are in memory per process: per hook (`rate_per_minute`, burst a quarter of it, at least 5) and 600 calls per minute per client address. Without `ANT_TRUSTED_PROXIES` the address behind an ingress is the ingress itself.

### Routing

The listener serves `/hook/` itself. The Helm chart routes the `/hook` prefix through both the Ingress and the HTTPRoute (as it does `/mcp`); other proxies must forward `/hook` to the same service. The console and API paths are unaffected.

### Metrics

`ant_incoming_hooks_total{hook,result}` with results `accepted`, `unauthorized`, `duplicate`, `too_large`, `rate_limited`, `succeeded`, `failed`. Unknown slugs are reported as `hook="_unknown"`.
