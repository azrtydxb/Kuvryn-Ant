# Nexus

Nexus Repository 3 can call a webhook for component and asset events.

Nexus payloads do not name their server, so the hook must say which Ant remote sources it covers (**Remote sources** or **Origins**). Saving is refused without one.

## 1. Create the hook

| Field          | Value                                                                                               |
| -------------- | --------------------------------------------------------------------------------------------------- |
| Slug           | `nexus-components`                                                                                  |
| Authentication | HMAC signature, format **Nexus**                                                                    |
| Signing secret | a secret from Access                                                                                |
| Action         | Upstream change notification, provider **Nexus**, sources: the remote sources that proxy this Nexus |

Nexus signs the body with **HMAC-SHA1** (header `X-Nexus-Webhook-Signature`, hex). That is a weaker hash than the other formats; the secret is only ever used to verify, so use a dedicated secret per sender.

## 2. Add the capability in Nexus

**Administration, System, Capabilities, Create capability, Webhook: Repository**:

| Field       | Value                                                     |
| ----------- | --------------------------------------------------------- |
| Repository  | the hosted or proxy repository                            |
| Event types | `component` created, deleted (and `asset` if you need it) |
| URL         | `https://ant.example.com/hook/nexus-components`           |
| Secret key  | the signing secret value                                  |

## What Ant does

For a component event the group and name are turned into package name hints (`org.acme:core`, `org/acme/core`, `org.acme/core`, `core`) and `version` is the version; for an asset event the directory of the asset path is used. Cached metadata whose resource name contains a hint is dropped from each covered source.
