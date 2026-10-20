# Docker Hub

Docker Hub tells Ant when an image tag is pushed, so Ant can drop the cached tag list and manifest answers of the matching Docker Hub remote source and, optionally, prewarm the new tag.

Docker Hub webhooks carry **no signature and no headers you control**, so the hook uses the **secret token in the URL** mode.

## 1. Create the hook

Console: **Webhooks, Incoming, Create incoming hook**.

| Field          | Value                                                 |
| -------------- | ----------------------------------------------------- |
| Slug           | `dockerhub-push`                                      |
| Authentication | Secret token in the URL                               |
| Action         | Upstream change notification, provider **Docker Hub** |

When you create it, Ant shows the full secret URL once:

```text
https://ant.example.com/hook/dockerhub-push/hk_<random>
```

Store it now. It is kept only as a digest; **Regenerate token** issues a new one and the old URL stops working at once.

## 2. Add the webhook in Docker Hub

Repository, **Webhooks**: name `ant`, Webhook URL the secret URL above.

With the Docker Hub API (a Hub JWT is required):

```sh
curl -fsS -X POST "https://hub.docker.com/v2/repositories/NAMESPACE/REPOSITORY/webhooks/" \
  -H "Authorization: JWT $HUB_JWT" -H 'Content-Type: application/json' \
  -d "{\"name\":\"ant\",\"expect_final_callback\":false,\"webhooks\":[{\"name\":\"ant\",\"hook_url\":\"$ANT_HOOK_URL\"}]}"
```

## What Ant does

Docker Hub posts `push_data.tag` and `repository.repo_name`. Ant matches remote sources of OCI format whose origin host is Docker Hub (`registry-1.docker.io`, `index.docker.io`, `docker.io`, `registry.hub.docker.com`), drops the cached metadata and negative-cache entries for `repo_name` (and `library/<name>` for official images) and, with **Also prewarm** enabled, prewarms `repo_name:tag` through the feeds that use the source. If you run a pull-through mirror of another registry, list its host under **Origins** instead.
