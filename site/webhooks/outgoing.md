# Outgoing webhooks

An outgoing webhook delivers Ant events (feed, source, token, policy and artifact changes, in the same transaction as the audit event) to your HTTPS endpoint, signed, with retries.

Outgoing webhooks are configured in the console only: **Webhooks, Outgoing, Create webhook**.

| Field          | Meaning                                                                                        |
| -------------- | ---------------------------------------------------------------------------------------------- |
| Name           | A label, 1 to 100 characters, unique on the instance                                           |
| Webhook URL    | The full target URL: https, no credentials, query or fragment, validated with the egress rules |
| Signing secret | From Access, or **+ Create new signing secret** from the form                                  |
| Enabled        | Pause delivery without deleting the webhook                                                    |

The egress rules reject `localhost` and private or special-use address literals when you save, and check every DNS answer again on each delivery connection. Configuration never makes a network request.

## Delivery

Each event is a `POST` with `X-Ant-Event-ID`, `X-Ant-Timestamp` and `X-Ant-Signature`, at least once, with up to 8 attempts and exponential backoff. The delivery history of a webhook lists each event with its state (`pending`, `delivered`, `dead`), attempts and failure class; a dead delivery can be redelivered. Consumers must deduplicate by event id.

## Verifying a delivery

The signature is HMAC-SHA256, keyed with the signing secret, over `timestamp + "\n" + event id + "\n" + body`. `X-Ant-Signature` is a comma-separated list of `sha256=<hex>` entries: the current secret first and, only inside a rotation grace window, the previous one. Accept the request when any entry matches a secret you hold, and reject old timestamps.

```sh
ts=$X_ANT_TIMESTAMP; id=$X_ANT_EVENT_ID
expected=sha256=$(printf '%s\n%s\n%s' "$ts" "$id" "$BODY" | openssl dgst -sha256 -hmac "$ANT_SIGNING_SECRET" -hex | sed 's/^.* //')
```

## Signing secrets

Rotate a secret with an optional grace window (up to 7 days) so receivers can switch without a gap; during it both values sign. A secret cannot be deleted while a webhook or an incoming hook uses it. See [Webhooks and signing secrets](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/docs/operations/webhooks.md) for the operator reference.

## Upgrading

Earlier builds had an instance webhook base URL and named webhooks. That is gone: every existing webhook keeps its endpoint, signing secret and history, its name becomes the display label, and webhooks that never had a name are labelled `webhook-<first 8 characters of the id>`. `ANT_WEBHOOK_BASE_URL` is ignored (with a startup warning).
