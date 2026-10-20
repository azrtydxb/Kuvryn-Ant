# Webhooks and signing secrets

## Model

- A **signing secret** (`/api/v1/webhook-secrets`, console: Access, Signing secrets) is a named HMAC key: id, unique name, description, created/rotated times, revision and the webhooks using it. The value is encrypted at rest through `internal/secrets` (purpose `webhook`, key bound to instance and record id), is returned once by the create or rotate call that produced it, and is never readable again. An idempotent replay of create or rotate returns the metadata without the value.
- A **webhook** references one signing secret. Named webhooks have a slug (`[a-z0-9-]`, 1 to 63 characters) and deliver to `webhook base URL + slug`. The resolved URL is stored in `endpoint`. **Custom-endpoint** webhooks keep a free-form HTTPS URL (administrator escape hatch, and every webhook that existed before migration 050).
- Rotating a secret affects every webhook that uses it. Deleting one is refused with 409 while any webhook references it.

## Base URL

`webhook_base_url` is an instance setting (Settings, General; `PUT /api/v1/settings/webhook-base-url`, its own route so it never interacts with the replace-all semantics of `PUT /api/v1/settings`). `ANT_WEBHOOK_BASE_URL` overrides it and makes it read-only. It must be https (`ANT_WEBHOOK_ALLOW_HTTP=true` also permits http), without credentials, query or fragment; one trailing slash is enforced so joining never doubles or drops a slash. Existing outbound rules still apply: no localhost or private/special-use address literals, and DNS answers are checked on every delivery connection.

Changing the base URL **re-resolves every named webhook in the same transaction**: the stored endpoint is rewritten, the webhook revision increases and each change is audited as `webhook.reresolve`. Custom endpoints are never touched. Clearing the base URL leaves existing endpoints as they are. When `ANT_WEBHOOK_BASE_URL` changes, the same re-resolution runs at startup.

## Rotation and signatures

`POST /api/v1/webhook-secrets/{id}/rotate` takes an optional `value` (omitted: 32 random bytes are generated) and optional `grace_seconds` (0 to 604800). With a grace window the previous value is kept as a separate encrypted record and **signs deliveries together with the new one** until the window ends; the expired record is deleted by the next secret mutation.

Each delivery is `POST` with `X-Ant-Event-ID`, `X-Ant-Timestamp` and `X-Ant-Signature`. The signature is HMAC-SHA256 over `timestamp + "\n" + event id + "\n" + body`. `X-Ant-Signature` is a comma-separated list of `sha256=<hex>` entries: the current secret first and, only inside a grace window, the previous one. Receivers must accept the request when any entry matches a secret they hold; a receiver that compares the whole header to one value breaks during a grace window, so deploy list-aware verification before using grace.

## Upgrade

Migration 050 turns each inline webhook secret into a signing secret named `webhook-<first 8 characters of the webhook id>` that adopts the same ciphertext row, so signatures do not change. Those webhooks are marked custom. The legacy `endpoint` + `secret` create body and `PUT /api/v1/webhooks/{id}/secret` still work (the latter rotates the shared secret the webhook uses, without grace).
