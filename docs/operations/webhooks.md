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
