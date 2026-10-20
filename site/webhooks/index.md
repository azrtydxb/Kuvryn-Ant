# Webhooks

Webhooks work in both directions. Console: **Webhooks**, with two tabs.

|              | Incoming                                                                                                   | Outgoing                                                               |
| ------------ | ---------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| Direction    | An external system calls `https://ant.example.com/hook/<slug>`                                             | Ant delivers its own events to your HTTPS endpoint                     |
| Typical uses | A registry or forge says "something changed"; CI asks Ant to prewarm, purge or replicate                   | Chat notifications, audit pipelines, automation reacting to Ant events |
| Signed with  | The sender's scheme (GitHub, GitLab, npm, Artifactory, Nexus, generic HMAC), a URL token or a bearer token | `X-Ant-Signature` (HMAC-SHA256) from a signing secret                  |
| Pages        | [Incoming hooks](./incoming), [Actions](./actions), [Provider setup](#provider-setup)                      | [Outgoing webhooks](./outgoing)                                        |

Both directions share the **signing secrets** under **Access, Signing secrets**: named HMAC keys that are encrypted at rest, shown once when created or rotated, and never readable again.

## Provider setup

Copy-paste setup for each sender:

- [GitHub](./github): releases and packages
- [GitLab](./gitlab): pushes, tags and releases
- [Docker Hub](./dockerhub): image pushes (secret token in the URL, because Docker Hub cannot sign)
- [Artifactory](./artifactory)
- [Nexus](./nexus)
- [npm](./npm)
- [Generic callers and CI](./generic): signed `curl`, bearer tokens

## Requirements

- `ANT_PUBLIC_URL` must be the externally reachable origin, because the console builds the hook URL from it.
- The Ingress or HTTPRoute must route `/hook` to Ant. The Helm chart does this by default (the same way it routes `/mcp`); see [Ingress, Gateway API and TLS](../install/ingress-tls).
- Managing hooks needs the instance **admin** permission. The receiver itself is public and authenticates every call itself.
