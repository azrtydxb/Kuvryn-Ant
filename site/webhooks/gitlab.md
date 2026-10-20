# GitLab

Push, tag push and release events of a GitLab project tell Ant when a remote source has new content.

## 1. Create the signing secret

Console: **Access, Signing secrets, Create signing secret**. GitLab sends the secret token as it is, so the value you copy is the value you give GitLab.

## 2. Create the hook

| Field          | Value                                             |
| -------------- | ------------------------------------------------- |
| Slug           | `gitlab-tags`                                     |
| Authentication | HMAC signature, format **GitLab**                 |
| Signing secret | the secret from step 1                            |
| Action         | Upstream change notification, provider **GitLab** |

Remote sources are matched by the host of the project URL in the payload (for example `gitlab.example.com`, plus `registry.gitlab.com` on gitlab.com).

## 3. Add the webhook in GitLab

Project, **Settings, Webhooks, Add new webhook**:

| Field        | Value                                         |
| ------------ | --------------------------------------------- |
| URL          | `https://ant.example.com/hook/gitlab-tags`    |
| Secret token | the signing secret value                      |
| Trigger      | Push events, Tag push events, Releases events |

Or with the API:

```sh
curl -fsS --request POST \
  --header "PRIVATE-TOKEN: $GITLAB_TOKEN" \
  "https://gitlab.example.com/api/v4/projects/$PROJECT_ID/hooks" \
  --data-urlencode "url=https://ant.example.com/hook/gitlab-tags" \
  --data-urlencode "token=$ANT_SIGNING_SECRET" \
  --data "push_events=true&tag_push_events=true&releases_events=true&enable_ssl_verification=true"
```

## Notes

- `X-Gitlab-Event-UUID` is remembered for 24 hours per hook, so a captured request cannot be replayed.
- GitLab compares the token itself, not a signature of the body. Ant compares it by digest in constant time, but treat the secret like a password and use a dedicated signing secret per sender.
