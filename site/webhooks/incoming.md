# Incoming hooks

An incoming hook is a URL on your Ant instance, `https://ant.example.com/hook/<slug>`, that an external system calls. Each hook has:

- a **slug** (lowercase letters, digits and hyphens, 1 to 63 characters, unique);
- a description and an enabled switch;
- one **authentication mode**;
- exactly one **[action](./actions)**;
- a rate limit (60 calls per minute by default).

The console form shows the live full URL, built from `ANT_PUBLIC_URL`, as you type the slug.

```text
https://ant.example.com/hook/release-notify
```

## Authentication

Choose one mode per hook.

### HMAC signature

The caller signs the raw request body with a signing secret from Access. Pick the format your sender uses:

| Format        | Header                      | What is compared                                   |
| ------------- | --------------------------- | -------------------------------------------------- |
| `github`      | `X-Hub-Signature-256`       | `sha256=<hex>`, HMAC-SHA256 of the body            |
| `gitlab`      | `X-Gitlab-Token`            | the shared token itself, compared by digest        |
| `ant`         | `X-Ant-Signature`           | `sha256=<hex>`, HMAC-SHA256 of the body (generic)  |
| `npm`         | `X-NPM-Signature`           | `sha256=<hex>`, HMAC-SHA256 of the body            |
| `artifactory` | `X-JFrog-Event-Auth`        | hex HMAC-SHA256 of the body, or the secret itself  |
| `nexus`       | `X-Nexus-Webhook-Signature` | hex HMAC-SHA1 of the body (the format Nexus sends) |

During a secret rotation with a grace window, both the new and the previous value are accepted. Docker Hub cannot sign at all: use the next mode for it.

### Secret token in the URL

Ant generates 256 random bits and the hook is served at `/hook/<slug>/<token>`. The token is shown **once**, in the console and in the create response; Ant stores only a SHA-256 digest. **Regenerate token** replaces it and the old URL stops working at once. Use this for senders that can neither sign nor send headers, such as Docker Hub. Treat the full URL as a secret: it can appear in proxy and server logs on the sender's side.

### Bearer Ant token

The caller sends `Authorization: Bearer ant_...`. The hook names a **scope** (`configure`, `replicate` or `admin`) and the token must hold that action on the instance, through both its scopes and its principal's roles. This suits CI jobs that already get Ant tokens, for example through [GitHub OIDC trust](../identity/github-oidc).

## What a caller sees

| Situation                                                               | Response                                      |
| ----------------------------------------------------------------------- | --------------------------------------------- |
| Accepted                                                                | `202` with `{"id":"...","status":"accepted"}` |
| Unknown slug, disabled hook, missing or wrong credential, bad signature | `404`, always the same body                   |
| Body larger than the cap (default 1 MiB)                                | `413`                                         |
| Provider delivery id seen in the last 24 hours                          | `409`                                         |
| Rate limit exceeded (after authentication)                              | `429` with `Retry-After`                      |

The 404 never says whether the slug exists or why the call was refused. Credential comparisons are constant time; the token and bearer modes are checked before the body is read.

The `202` means the call was authenticated and queued. The action runs afterwards in a worker that only runs under the instance writer lease, so a slow action never holds the caller.

## Replay protection and limits

- `X-GitHub-Delivery`, `X-Gitlab-Event-UUID`, `X-Nexus-Webhook-Delivery` and `X-Ant-Delivery-Id` are remembered for **24 hours** per hook. A repeat is answered `409` and logged as `duplicate`. GitHub's "Redeliver" button reuses the delivery id, so use the console's redelivery for testing instead.
- The body cap is `ANT_HOOK_MAX_BODY_BYTES` (default 1 MiB, at most 8 MiB). It is checked on the declared length before anything is read, and again while reading.
- Rate limits are per hook (`rate_per_minute`, burst of a quarter of it, at least 5) and per client address across all hooks (600 per minute). Counters are per process.

## Delivery log

Every call to an existing hook is recorded: time, source address, authentication result (`ok`, `invalid`, `missing`, `duplicate`, `too_large`, `test`, `redelivery`), state, action result, attempts, size and a **payload preview**. The preview is truncated to 2 KiB and redacted: values under keys such as `token`, `secret`, `password`, `authorization` or `signature` and credential-shaped strings (`ant_...`, `ghp_...`, bearer tokens, JWTs) are replaced by `[redacted]`.

- Rejected calls keep no payload and are logged at most 60 times a minute per hook.
- Deliveries are kept for `ANT_HOOK_LOG_RETENTION_DAYS` days (default 14).
- **Redeliver** queues a copy of a stored delivery, to run the action again while you build or debug the hook. **Send test** posts a sample payload for the hook's action; for HMAC hooks it is signed with the hook's real secret and verified like a live call.
- The hook list shows the full URL (copyable), authentication, action, last received time and the seven-day success rate.

## Metrics and audit

`ant_incoming_hooks_total{hook,result}` counts calls per hook slug. `result` is `accepted`, `unauthorized`, `duplicate`, `too_large`, `rate_limited` and, when the worker finishes a delivery, `succeeded` or `failed`. Unknown slugs are folded into `hook="_unknown"` so a scanner cannot create series.

Creating, changing, regenerating the token of and deleting a hook are audited (`incoming_hook.create`, `.patch`, `.token`, `.delete`), and so are the state-changing actions a hook runs.

## API

All under `/api/v1`, instance admin only, `Idempotency-Key` on mutations, `If-Match` on changes.

| Method and path                                                | Purpose                                                 |
| -------------------------------------------------------------- | ------------------------------------------------------- |
| `GET /incoming-hook-config`                                    | Public base URL, size cap, retention, signature formats |
| `GET /incoming-hooks`, `POST /incoming-hooks`                  | List with statistics, create                            |
| `GET`, `PATCH`, `DELETE /incoming-hooks/{id}`                  | Read, change, delete                                    |
| `POST /incoming-hooks/{id}/token`                              | Regenerate the URL token (returned once)                |
| `GET /incoming-hooks/{id}/deliveries`                          | The delivery log, newest first                          |
| `POST /incoming-hooks/{id}/test`                               | Queue a sample payload                                  |
| `POST /incoming-hooks/{id}/deliveries/{delivery_id}/redeliver` | Queue a copy of a logged delivery                       |

```sh
export ANT=https://ant.example.com
curl -fsS -X POST $ANT/api/v1/incoming-hooks \
  -H "Authorization: Bearer $ANT_TOKEN" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: hook-release-notify-1' -d '{
    "slug": "release-notify",
    "description": "GitHub releases",
    "auth_mode": "hmac",
    "hmac_format": "github",
    "signing_secret_id": "<SIGNING_SECRET_ID>",
    "action": {"type": "upstream_notify", "provider": "github"}
  }'
```
