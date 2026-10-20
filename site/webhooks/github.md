# GitHub

Releases and packages of a GitHub repository tell Ant when a remote source has new content. Supported events: `release`, `package` and `registry_package`.

## 1. Create the signing secret

Console: **Access, Signing secrets, Create signing secret**. Copy the value when it is shown; it is shown once.

## 2. Create the hook

Console: **Webhooks, Incoming, Create incoming hook**.

| Field          | Value                                             |
| -------------- | ------------------------------------------------- |
| Slug           | `release-notify`                                  |
| Authentication | HMAC signature, format **GitHub**                 |
| Signing secret | the secret from step 1                            |
| Action         | Upstream change notification, provider **GitHub** |

The hook URL is `https://ant.example.com/hook/release-notify`. Remote sources are matched by origin: `github.com` for releases, `ghcr.io` for container packages and `npm.pkg.github.com` (and its siblings) for other package types. List `origins` or sources in the action to override.

## 3. Add the webhook in GitHub

Repository, **Settings, Webhooks, Add webhook**:

| Field        | Value                                         |
| ------------ | --------------------------------------------- |
| Payload URL  | `https://ant.example.com/hook/release-notify` |
| Content type | `application/json`                            |
| Secret       | the signing secret value                      |
| Events       | Releases, and Packages if you use GHCR        |

Or with the GitHub CLI:

```sh
gh api repos/OWNER/REPO/hooks \
  -f name=web \
  -f 'config[url]=https://ant.example.com/hook/release-notify' \
  -f 'config[content_type]=json' \
  -f "config[secret]=$ANT_SIGNING_SECRET" \
  -f 'events[]=release' -f 'events[]=package'
```

GitHub sends a `ping` event when the webhook is created. Ant accepts and ignores it, so it shows as `ignored` in the log.

## Notes

- Add `X-GitHub-Delivery` replay protection for free: a delivery id is remembered for 24 hours. GitHub's **Redeliver** button resends the same id and is answered `409`; replay from the Ant console instead.
- To prewarm the new tag as well, enable **Also prewarm the changed tag or version** on the action.
- Instead of invalidating caches you can start a prewarm directly with a `prewarm` action and a template such as <code v-pre>{{ .release.tag_name | trimv }}</code>.
