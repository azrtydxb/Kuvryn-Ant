# Security policy

## Reporting a vulnerability

Please do **not** report security vulnerabilities through public GitHub issues,
discussions or pull requests.

Report them privately through GitHub's private vulnerability reporting:

1. Open <https://github.com/azrtydxb/Kuvryn-Ant/security/advisories/new>
   (repository **Security** tab, then **Report a vulnerability**).
2. Describe the issue, the affected version or commit, the configuration needed to
   reproduce it, and the impact you expect.
3. Include a proof of concept if you have one. Do not include real credentials,
   tokens or customer data.

You will receive an acknowledgement within 5 working days. We will keep you
informed while we investigate, agree on a disclosure date with you, and credit you
in the advisory unless you prefer to stay anonymous.

## Supported versions

Kuvryn Ant has not published a stable release yet. Security fixes are made on the
`main` branch. Once releases exist, the latest minor release line will receive
security fixes.

| Version         | Supported   |
| --------------- | ----------- |
| `main`          | yes         |
| 0.x pre-release | best effort |

## Scope

In scope: the `ant` server and CLI (`cmd/`, `internal/`), the management API, the
web console, the Helm chart in `deploy/chart`, the container image built from the
`Dockerfile`, and the documentation's recommended configurations.

Out of scope: vulnerabilities in upstream registries or package clients, denial of
service that requires administrator credentials, and findings that depend on an
operator disabling documented protections (for example exposing `/metrics`
publicly or enabling `ANT_S3_ALLOW_INSECURE_HTTP` on an untrusted network).

## Security model

The documentation describes how credentials, tokens, upstream origins and audit are
protected: <https://azrtydxb.github.io/Kuvryn-Ant/about/security>.
