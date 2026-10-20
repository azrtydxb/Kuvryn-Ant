# Namespaces and scopes

A **namespace** is a string each format adapter derives from a package's identity, for example the npm scope or the Maven groupId. Ant stores it with every component. Grants, token scopes, publish selectors, quotas and delete policies can match it, which lets you narrow access to part of a feed without creating more feeds.

## Where namespaces are used

| Feature          | Field                                                                              | Matching                                                                                                  |
| ---------------- | ---------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------- |
| Role grants      | `namespace` on an `artifact` grant (`PUT /api/v1/roles/{id}/grants`)               | Exact. An absent namespace selects every namespace.                                                       |
| Token scopes     | `namespace` in a service token, PAT or GitHub OIDC trust-rule scope (`TokenScope`) | Exact. An absent namespace selects every namespace. Scopes only narrow what the principal's grants allow. |
| Publish policies | `selector.namespace` in a [publish rule](publish-policies.md)                      | Exact and case-sensitive. Empty is unconstrained.                                                         |
| Quotas           | `QuotaRule` with `scope: namespace` and a `feed_id`                                | Bounds bytes and package count of one namespace of a feed                                                 |
| Rate limits      | `RateLimitRule` with scope `namespace` (subject is the feed UUID)                  | Per namespace of a feed                                                                                   |
| Delete policies  | `namespace` on a feed delete policy                                                | Per namespace                                                                                             |
| Resolve explain  | `namespace` query parameter of `GET /api/v1/feeds/{id}/resolve`                    | An owner under a different namespace reports `namespace_mismatch`                                         |

Namespaces never imply descendants. A grant or scope for `team` does not match `team/child`. For OCI, where the namespace is a path, grant each namespace you need explicitly.

An empty namespace on a grant, scope or selector means "any namespace". You therefore cannot write a grant that matches only packages without a namespace (for example unscoped npm packages). Use a feed-wide grant for those.

## Namespace per format

The value is computed by the adapter from the request. It is not configurable.

| Format                                                       | Namespace                                                      | Example: package to namespace                                                                                                                                                                                                                                                       |
| ------------------------------------------------------------ | -------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| OCI                                                          | Repository path inside the feed, up to its last `/`            | `ant.example.com/buildcache/myorg/myrepo` gives `myorg`. `ant.example.com/oci-hosted/team/svc/api` gives `team/svc`. A single-segment repository has an empty namespace. On Docker Hub mirror feeds, reads of `alpine` are canonicalised to `library/alpine` (namespace `library`). |
| npm                                                          | The scope including `@`, or empty for unscoped packages        | `@myorg/ui` gives `@myorg`. `lodash` gives an empty namespace.                                                                                                                                                                                                                      |
| Maven                                                        | `groupId`                                                      | `org.example:lib` gives `org.example`                                                                                                                                                                                                                                               |
| Go                                                           | Module path up to its last `/`                                 | `github.com/myorg/mod` gives `github.com/myorg`                                                                                                                                                                                                                                     |
| Composer                                                     | Vendor                                                         | `myorg/tools` gives `myorg`                                                                                                                                                                                                                                                         |
| Ansible Galaxy                                               | Collection namespace                                           | `myorg.utils` gives `myorg`                                                                                                                                                                                                                                                         |
| Swift                                                        | Package scope (lowercase)                                      | `MyOrg.Kit` gives `myorg`                                                                                                                                                                                                                                                           |
| Conan                                                        | Reference user (empty for `_`)                                 | `zlib/1.3/myorg/stable` gives `myorg`                                                                                                                                                                                                                                               |
| Raw                                                          | Directory of the file path                                     | `releases/tool/1.2/tool.tar.gz` gives `releases/tool/1.2`                                                                                                                                                                                                                           |
| Conda                                                        | Channel subdir                                                 | `linux-64`, `noarch`                                                                                                                                                                                                                                                                |
| APK                                                          | `<branch>/<repo>/<arch>`                                       | `v3.20/main/x86_64`                                                                                                                                                                                                                                                                 |
| CRAN                                                         | `src/contrib`                                                  | constant                                                                                                                                                                                                                                                                            |
| Terraform                                                    | `modules` or `providers`                                       | constant per kind                                                                                                                                                                                                                                                                   |
| APT                                                          | `pool`, `dists` (and `apt-remote` for cached upstream indexes) | constant per kind                                                                                                                                                                                                                                                                   |
| RPM                                                          | `rpm`                                                          | constant                                                                                                                                                                                                                                                                            |
| P2                                                           | `p2`                                                           | constant                                                                                                                                                                                                                                                                            |
| Hugging Face                                                 | `huggingface`                                                  | constant                                                                                                                                                                                                                                                                            |
| Git LFS                                                      | `git-lfs`                                                      | constant                                                                                                                                                                                                                                                                            |
| PyPI, NuGet, RubyGems, Cargo, Helm (classic), pub, CocoaPods | Empty                                                          | Not namespaced                                                                                                                                                                                                                                                                      |

For formats with a constant namespace, or with none, namespace-scoped grants and scopes add nothing. Use feed-level grants instead, or separate feeds.

::: warning Gap
The table was derived from the adapters' source code (the `Namespace` they set on each publication), not from per-format documentation. Edge cases are not covered by tests documented here. For example, a Go module path without a slash (`example.com`) yields the namespace `.`, and casing rules beyond Swift's lowercasing have not been checked for every format. Confirm with `GET /api/v1/feeds/{id}/resolve` or the console package view before relying on a namespace-scoped grant.
:::

## Examples

### A CI token limited to one OCI namespace

A token for repository `myorg/myrepo` in feed `buildcache` can be limited to the namespace `myorg`:

```json
{
  "scopes": [
    {
      "feed_id": "<buildcache feed id>",
      "namespace": "myorg",
      "actions": ["read", "publish"]
    }
  ]
}
```

The token can push `ant.example.com/buildcache/myorg/myrepo:main-amd64`. It cannot push `ant.example.com/buildcache/other/repo`, and it cannot push `ant.example.com/buildcache/myorg/sub/repo`, whose namespace is `myorg/sub`.

### A team grant for an npm scope

```json
{
  "grants": [
    {
      "kind": "artifact",
      "effect": "allow",
      "feed_id": "<npm feed id>",
      "namespace": "@team-a",
      "actions": ["read", "publish"]
    }
  ]
}
```

Combine it with a publish rule that routes `@team-a` to the team's local source. The grant decides whether the principal may publish, and the rule decides where the publish goes.

### A Maven group quota

```json
{
  "scope": "namespace",
  "feed_id": "<maven feed id>",
  "namespace": "org.example",
  "max_bytes": 10737418240
}
```

## Related

- [Roles and grants](../identity/roles.md)
- [Service accounts and tokens](../identity/tokens.md)
- [GitHub OIDC trust](../identity/github-oidc.md)
- [Publish policies](publish-policies.md)
