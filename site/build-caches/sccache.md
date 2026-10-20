# sccache over WebDAV

[sccache](https://github.com/mozilla/sccache) is a compiler wrapper that caches compilation results (Rust, and C/C++ through gcc, clang and MSVC). Its WebDAV storage backend can use a Kuvryn Ant raw [cache feed](index.md) as a shared remote cache, so ephemeral CI runners reuse each other's compilation results.

The endpoint is the raw feed's path: `https://ant.example.com/feeds/sccache/raw`.

## What Ant implements

The raw adapter implements the WebDAV subset that sccache's OpenDAL client uses ([`internal/rawrepo/webdav.go`](https://github.com/azrtydxb/Kuvryn-Ant/blob/main/internal/rawrepo/webdav.go)):

| Method        | Behaviour                                                                                                                                                                                                                        |
| ------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `OPTIONS`     | `204` with `DAV: 1` and `Allow: OPTIONS, GET, HEAD, PUT, PROPFIND, MKCOL`                                                                                                                                                        |
| `MKCOL`       | Checks that the caller may publish under the path and answers `201`. Collections are virtual: nothing is stored, a directory exists while a file lives below it. A body is refused with `415`.                                   |
| `PUT`         | Stores the file (the normal raw publish). A `PUT` with **different** bytes for an existing key is `409`, which sccache logs and ignores.                                                                                         |
| `GET`, `HEAD` | The normal raw download.                                                                                                                                                                                                         |
| `PROPFIND`    | Existence, size, ETag, content type and `getlastmodified` of a file, or existence of a collection. Children of a collection are not listed. The feed root always exists; any other collection exists once a file lives below it. |
| `DELETE`      | The plain raw delete, gated by the feed's delete policy.                                                                                                                                                                         |

`COPY`, `MOVE` and locking are not supported; sccache does not need them. A file path (the part after `/feeds/<slug>/raw/`) can be at most 256 characters.

Tested with the real sccache 0.17 client and cargo (`go run ./scripts/cachesmoke`): a cold build wrote its entries, a build after `cargo clean` hit all of them with no cache errors, with both bearer and Basic authentication.

## Authentication

Ant accepts the token as a Bearer token or as the Basic password with any username, so both sccache options work:

::: code-group

```sh [Bearer token]
export SCCACHE_WEBDAV_ENDPOINT=https://ant.example.com/feeds/sccache/raw
export SCCACHE_WEBDAV_TOKEN=$ANT_TOKEN
```

```sh [Basic auth]
export SCCACHE_WEBDAV_ENDPOINT=https://ant.example.com/feeds/sccache/raw
export SCCACHE_WEBDAV_USERNAME=ci
export SCCACHE_WEBDAV_PASSWORD=$ANT_TOKEN
```

:::

The token needs `read` and `publish` on the sccache feed. Without `publish`, sccache can read hits but every write fails (sccache logs it and continues). See [Cache feeds and policies](index.md#grant-ci-access) for the grants.

## Rust

```sh
export RUSTC_WRAPPER=sccache
export CARGO_INCREMENTAL=0
export SCCACHE_WEBDAV_ENDPOINT=https://ant.example.com/feeds/sccache/raw
export SCCACHE_WEBDAV_TOKEN=$ANT_TOKEN
export SCCACHE_WEBDAV_KEY_PREFIX=myorg/myrepo

cargo build --locked
sccache --show-stats
```

- `CARGO_INCREMENTAL=0`: sccache does not cache incremental compilation, so turn it off in CI (debug profiles enable it by default).
- Per sccache's documentation, crates that invoke the system linker (`bin`, `dylib`, `cdylib`, `proc-macro`) are not cached; dependencies (`lib`/`rlib`) are, which is where most CI time goes.
- sccache reads its configuration when its server starts. If a server is already running with other settings, run `sccache --stop-server` first.

In GitHub Actions, [`ant-setup-action`](../ci/github-actions.md#sccache-over-webdav) with `build-cache: "true"` installs sccache (checksum verified) and exports exactly these variables with a short-lived OIDC token.

## C and C++

sccache wraps C/C++ compilers too. These are standard sccache usages; the Ant side is the same WebDAV endpoint and variables as above.

::: code-group

```sh [CMake]
cmake -B build -DCMAKE_C_COMPILER_LAUNCHER=sccache -DCMAKE_CXX_COMPILER_LAUNCHER=sccache
cmake --build build
```

```sh [Make / autotools]
export CC="sccache gcc" CXX="sccache g++"
./configure && make -j"$(nproc)"
```

:::

::: warning Not verified for C/C++
The repository tests sccache only with cargo/rustc. C/C++ caching over Ant's WebDAV endpoint uses the same protocol and is expected to work, but it is not covered by Ant's tests. Check the hit rate with `sccache --show-stats`.
:::

## Key prefixes and sharing

`SCCACHE_WEBDAV_KEY_PREFIX` is prepended to every cache key, which becomes the file path in the raw feed.

- sccache keys are content hashes of the compiler, its arguments and inputs, so entries are valid across branches. A **per-repository** prefix (`myorg/myrepo`) shares them between branches and is the right default; `ant-setup-action` uses `<org>/<repo>`.
- A **per-branch** prefix (`myorg/myrepo/<branch>`) isolates untrusted pull requests at the cost of sharing. It also makes branch cleanup possible: `DELETE /api/v1/feeds/{id}/cache?prefix=myorg/myrepo/<branch>/` removes that branch's files. With a per-repository prefix there is nothing branch-specific to remove; idle eviction handles it.
- A prefix per toolchain or target is unnecessary: the compiler version and target are part of the hash.

Keep the prefix short; the full path (prefix plus sccache's key) must stay within the 256-character raw path limit.

## Cache statistics

```sh
sccache --show-stats
```

Look at "Cache hits", "Cache misses" and "Cache errors". Cache errors with a remote backend usually mean authentication problems (expired token, missing `publish`) or an unreachable endpoint. sccache fails soft: the build passes even when every cache operation fails, so check the statistics rather than the build result.

In GitHub Actions, the [`cache-stats`](../ci/github-actions.md#sub-action-cache-stats) sub-action prints the statistics, writes hits and misses to the job summary, exposes them as outputs, and can fail the job below `min-hits`.

On the Ant side, the feed's bytes are shown on its Settings page and in `ant_feed_used_bytes{purpose="cache",feed="sccache"}`; evictions appear in `ant_storage_evicted_bytes_total{purpose="cache"}`.

## Sizing the policy

sccache writes many small files and never deletes them. Set the feed's `max_idle_seconds` (for example 7 days) so entries of old toolchains and dependency versions age out, and `max_bytes` to cap the feed; least recently used files are evicted first. Entries that a build reads again are refreshed by the access and survive. See [Cache policy](index.md#cache-policy).

## Troubleshooting

| Symptom                                     | Cause                                                                                                                                                                                                           |
| ------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 0 hits on a second build, many cache errors | Token missing or expired, wrong endpoint (it must end in `/feeds/<slug>/raw`), or the feed is not a raw feed.                                                                                                   |
| 0 hits, no errors                           | `RUSTC_WRAPPER` not set in the build's environment, `CARGO_INCREMENTAL` left on, a running sccache server started before the variables were set, or a different key prefix than the build that wrote the cache. |
| `409` in sccache logs                       | A `PUT` of different bytes to an existing key; harmless (non-deterministic outputs from two builders).                                                                                                          |
| `404` on everything                         | The token cannot read the feed (Ant answers `404` to callers without read), or the slug is wrong.                                                                                                               |

::: warning Gap

- C/C++ use of sccache over Ant is not tested in the repository.
- Children of a WebDAV collection are not listed by `PROPFIND`; WebDAV clients other than sccache that need directory listings will not work.
  :::
