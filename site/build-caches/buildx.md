# buildx registry cache

BuildKit can export its layer cache to a registry (`--cache-to type=registry`) and import it in the next build (`--cache-from type=registry`). Pointing that at an OCI [cache feed](index.md) gives ephemeral CI builders a warm cache: unchanged Dockerfile steps are reported as `CACHED` instead of being rebuilt.

The cache lives at `ant.example.com/<cache feed>/<repository>:<tag>`, for example `ant.example.com/buildcache/myorg/myrepo:main-amd64`. The first path segment is the feed slug, the rest is the repository name, as for any Ant OCI feed (see [OCI / Docker](../formats/oci.md)).

## Authenticate

The builder needs a token with `read` and `publish` on the cache feed (and `delete` if the same identity cleans up branches). Docker credentials are used by the buildx `docker-container` driver too:

```sh
echo "$ANT_TOKEN" | docker login ant.example.com -u ci --password-stdin
```

Any username works; the token is the password. In GitHub Actions, [`ant-setup-action`](../ci/github-actions.md) with `build-cache: "true"` performs the login with a short-lived OIDC token. See [Package client auth](../identity/client-auth.md).

## Command line

```sh
REF=ant.example.com/buildcache/myorg/myrepo
BRANCH=$(git rev-parse --abbrev-ref HEAD | tr -c 'A-Za-z0-9_.\n-' '-')
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')

docker buildx build \
  --cache-from type=registry,ref=$REF:$BRANCH-$ARCH \
  --cache-from type=registry,ref=$REF:main-$ARCH \
  --cache-to type=registry,ref=$REF:$BRANCH-$ARCH,mode=max,image-manifest=true,ignore-error=true \
  -t ant.example.com/oci-hosted/myorg/myrepo:dev .
```

### Required and recommended options

| Option                | Why                                                                                                                                                                                                                                                                                          |
| --------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `image-manifest=true` | **Required.** BuildKit's default cache export is an OCI image index whose entries are layers, which a registry that validates indexes (Ant included) refuses. With `image-manifest=true` the cache is an ordinary OCI image manifest whose config uses the BuildKit cache config media type. |
| `mode=max`            | Exports the cache of every stage, including intermediate stages of multi-stage builds, not only the layers of the final image. Larger uploads, many more hits.                                                                                                                               |
| `ignore-error=true`   | A cache outage, a full feed or an expired token does not fail the build; the export is skipped.                                                                                                                                                                                              |
| `oci-mediatypes=true` | BuildKit requires OCI media types for `image-manifest=true`; it is BuildKit's default, so you normally do not need to set it. Do not set it to `false`.                                                                                                                                      |

`image-manifest=true` with `mode=max` and `ignore-error=true` is the combination tested in the repository (`scripts/cachesmoke`, with the `docker-container` driver).

## Per-branch refs with a fallback to main

The tag is the cache identity. Use one tag per branch and architecture, `<branch>-<arch>`, and import the default branch's cache as a fallback so a new branch starts warm:

- Tags allow `[A-Za-z0-9_.-]` and at most 128 characters: replace the slashes of a branch name (`feature/x` becomes `feature-x`).
- List the branch ref first, then `main`; BuildKit uses whatever it finds.
- Export only to the branch ref.
- Tag builds (releases) can share the default branch's cache instead of creating one cache per tag; `ant-setup-action` does that.
- A repository that builds several images adds a name: `<branch>-<name>-<arch>`.

Re-pushing the same tag is the normal case. In a cache feed the tag moves to the new manifest, and the previous manifest and the layers nothing else references are released at once and freed by the short sweep (see [Tag overwrite releases old content](index.md#tag-overwrite-releases-old-content)). You do not need to clean up between builds.

::: tip Missing refs
A `--cache-from` ref that does not exist yet is a warning for plain `docker buildx build` and for `docker/build-push-action` v7, but fatal for `docker/build-push-action` v6. `ant-setup-action` lists only refs that exist; if you build refs yourself for v6, check first or upgrade to v7.
:::

## GitHub Actions with docker/build-push-action

With `ant-setup-action` (recommended; OIDC, no stored secret):

```yaml
permissions:
  id-token: write
  contents: read
jobs:
  image:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: docker/setup-buildx-action@v4
      - id: ant
        uses: azrtydxb/ant-setup-action@<sha> # v1.2.0
        with:
          host: ant.example.com
          build-cache: "true"
          go: "false"
          npm: "false"
          cargo: "false"
          pip: "false"
      - uses: docker/build-push-action@v7
        with:
          context: .
          push: true
          tags: ant.example.com/oci-hosted/myorg/myrepo:${{ github.sha }}
          cache-from: ${{ steps.ant.outputs.cache-from }}
          cache-to: ${{ steps.ant.outputs.cache-to }}
```

The action computes `cache-from` (branch tag, then default-branch tag, only those that exist) and `cache-to` (`mode=max,image-manifest=true,ignore-error=true`). See [GitHub Actions](../ci/github-actions.md#buildx-registry-cache) for the tag rules.

Without the action, with a stored token:

```yaml
jobs:
  image:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: docker/setup-buildx-action@v4
      - uses: docker/login-action@v3
        with:
          registry: ant.example.com
          username: ci
          password: ${{ secrets.ANT_TOKEN }}
      - id: tag
        run: |
          b="${GITHUB_HEAD_REF:-$GITHUB_REF_NAME}"
          echo "branch=$(printf '%s' "$b" | tr -c 'A-Za-z0-9_.-' '-')" >> "$GITHUB_OUTPUT"
      - uses: docker/build-push-action@v7
        with:
          context: .
          push: true
          tags: ant.example.com/oci-hosted/myorg/myrepo:${{ github.sha }}
          cache-from: |
            type=registry,ref=ant.example.com/buildcache/myorg/myrepo:${{ steps.tag.outputs.branch }}-amd64
            type=registry,ref=ant.example.com/buildcache/myorg/myrepo:main-amd64
          cache-to: type=registry,ref=ant.example.com/buildcache/myorg/myrepo:${{ steps.tag.outputs.branch }}-amd64,mode=max,image-manifest=true,ignore-error=true
```

## Read-only caches for untrusted builds

Pull requests from forks should not write the cache that the default branch reads. Options:

- Give pull-request builds no `cache-to` (`cache-write: "false"` in `ant-setup-action`), and a token with only `read` on the cache feed (a separate GitHub OIDC trust rule for non-default refs).
- Or let them write only their own branch tags and rely on [branch cleanup](index.md#branch-cleanup) and the idle policy.

## Clean up

When a branch is deleted or its pull request closes, remove its tags with `DELETE /api/v1/feeds/{id}/cache?prefix=myorg/myrepo:<branch>-` or the [`cache-cleanup`](../ci/github-actions.md#sub-action-cache-cleanup) sub-action. Caches nobody touches are evicted by the feed's `max_idle_seconds` anyway.

## Troubleshooting

| Symptom                                                      | Cause                                                                                                                                           |
| ------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| Cache export fails with a manifest or index validation error | `image-manifest=true` is missing.                                                                                                               |
| `401` / `403` on export                                      | Not logged in, token expired, or no `publish` on the cache feed (or the OCI namespace does not match the token's scope).                        |
| `409` on the second export to the same tag                   | The target is an `artifacts` feed, where tags are immutable (the first export succeeds, re-exporting fails). Use a cache feed.                  |
| `MANIFEST_BLOB_UNKNOWN` on export, build still green         | A concurrent build's tag overwrite released a shared layer; the export was skipped (`ignore-error=true`).                                       |
| Every step rebuilds although the export succeeded            | The import ref differs from the export ref (branch name sanitized differently, other architecture), or the builder runs without `--cache-from`. |

::: warning Gap
Only `image-manifest=true` together with `mode=max` and `ignore-error=true` is documented and tested in the repository. `oci-mediatypes`, `compression` and other BuildKit cache options are not tested against Ant; the `type=registry` default export (an index of layers, without `image-manifest=true`) is refused.
:::
