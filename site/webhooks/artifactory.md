# Artifactory

An Artifactory webhook tells Ant when an artifact is deployed or an image is pushed upstream, so the matching remote source sees it immediately.

Artifactory payloads do not name their server, so the hook must say which Ant remote sources it covers (**Remote sources** or **Origins** in the action). Saving is refused without one.

## 1. Create the hook

| Field          | Value                                                                                                           |
| -------------- | --------------------------------------------------------------------------------------------------------------- |
| Slug           | `artifactory-deploys`                                                                                           |
| Authentication | HMAC signature, format **Artifactory**                                                                          |
| Signing secret | a secret from Access                                                                                            |
| Action         | Upstream change notification, provider **Artifactory**, sources: the remote sources that proxy this Artifactory |

Ant accepts the `X-JFrog-Event-Auth` header either as a hex HMAC-SHA256 of the body (with or without a `sha256=` prefix) or, as older servers send it, the secret itself.

## 2. Add the webhook in Artifactory

**Administration, Platform Management, Event Subscriptions (Webhooks), New Webhook**: URL `https://ant.example.com/hook/artifactory-deploys`, the secret, and the events _Artifact was deployed_ and _Docker tag was pushed_ for the repositories you proxy.

Or with the REST API (check the field names against your Artifactory version):

```sh
curl -fsS -X POST "https://artifactory.example.com/event/api/v1/subscriptions" \
  -H "Authorization: Bearer $JFROG_TOKEN" -H 'Content-Type: application/json' -d '{
    "key": "ant-deploys",
    "enabled": true,
    "event_filter": {
      "domain": "artifact",
      "event_types": ["deployed", "deleted"],
      "criteria": {"anyLocal": true, "anyRemote": false, "anyFederated": false, "repoKeys": []}
    },
    "handlers": [{
      "handler_type": "webhook",
      "url": "https://ant.example.com/hook/artifactory-deploys",
      "secret": "'"$ANT_SIGNING_SECRET"'",
      "use_secret_for_signing": true
    }]
  }'
```

## What Ant does

For an `artifact` event, the directories of the deployed path and the file name are used as the package name hints (for a Maven path `org/acme/core/2.0/core-2.0.jar`: `org/acme/core`, `org/acme/core/2.0`, `core-2.0.jar`) and the version directory as the version. For a `docker` event the image name and tag are used. Cached metadata whose resource name contains one of the hints is dropped. This is a heuristic: it can drop a little more than needed, never less.
