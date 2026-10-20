# Measurements

These are timings the project reported from its own CI after moving builds onto Kuvryn Ant cache feeds. They are observations from real pipelines, not controlled benchmarks: each compares a job before and after, or cold and warm, on the same project. Your results depend on the build, the runner hardware and the network path to Ant.

## Reported results

| Project and job                    | Cache                                      | Before | After     | Notes                                              |
| ---------------------------------- | ------------------------------------------ | ------ | --------- | -------------------------------------------------- |
| Fastllm-proxy, test job (Rust)     | sccache over WebDAV in a raw cache feed    | 10m55s | 4m41s     | 751 of 751 compilations were sccache hits          |
| NovaCRM, Docker image build        | buildx registry cache in an OCI cache feed | 9m42s  | 3s (warm) | warm: the build was served from the registry cache |
| novanet, `cargo clippy` job (Rust) | sccache over WebDAV in a raw cache feed    | 1m08s  | 21.5s     |                                                    |

## What was cached

### Rust jobs: sccache over WebDAV

The Fastllm-proxy and novanet jobs use sccache with `RUSTC_WRAPPER=sccache`, `CARGO_INCREMENTAL=0` and `SCCACHE_WEBDAV_ENDPOINT` pointing at the raw cache feed, as set by `ant-setup-action` with `build-cache: "true"` (see [sccache over WebDAV](sccache.md)). sccache stores the output of each `rustc` invocation under a hash of the compiler, arguments and inputs. A second build with the same toolchain and dependency versions downloads those outputs instead of compiling.

"751/751 hits" means every compilation sccache was asked to perform was found in the cache. What remains in the 4m41s is work sccache does not cache: crate downloads, linking of binaries and test executables (per sccache's documentation, crates that invoke the linker are not cacheable), build scripts that must run, and running the tests themselves.

For the clippy job the report gives only the timings; the split between cached dependency builds and the remaining lint work is not part of the report.

### Docker build: buildx registry cache

The NovaCRM image build uses the buildx registry cache in an OCI cache feed. The recommended setup, which `ant-setup-action` generates, exports the BuildKit cache with `--cache-to type=registry,ref=<host>/buildcache/<org>/<repo>:<branch>-<arch>,mode=max,image-manifest=true,ignore-error=true` and imports it with `--cache-from` for the branch tag and the default branch tag (see [buildx registry cache](buildx.md)). `mode=max` also exports the cache of intermediate build stages.

## What "warm" means

- **Docker, warm:** the cache tag for the branch (or the default-branch fallback) already exists in the cache feed from an earlier build, and the Dockerfile and its build context did not change in a way that invalidates the steps. BuildKit then reports every step as `CACHED` and only resolves cache metadata; nothing is rebuilt. The 3s figure is such a build. A change early in the Dockerfile, or to files copied in early, invalidates every step after it, and those steps run at normal speed.
- **Rust:** the sccache entries for the same toolchain, target, profile and dependency versions exist under the job's key prefix. The first build after a toolchain upgrade or a `Cargo.lock` change misses for the affected crates and repopulates the cache.

The "before" columns are the reported durations of the same jobs before the remote cache was in use. The report does not break them down further.

## Caveats

- **Not benchmarks.** Single reported runs, not repeated measurements with variance; no runner or network specification is attached to them.
- **The best case.** A fully warm Docker build (3s) is the upper bound of the gain. Typical pull-request builds change something and land between cold and warm.
- **Ephemeral runners benefit most.** Runners that keep their Docker layer store or `target/` directory between jobs see smaller gains, because they already have a local cache.
- **Cache transfer costs time too.** With `mode=max`, exporting the cache uploads every intermediate layer; the export time is part of the job, and a slow path to Ant reduces the gain.
- **sccache fails soft.** A misconfigured token or endpoint gives zero hits and no build failure; check `sccache --show-stats` (or the `cache-stats` sub-action) when you measure.
- **Eviction.** Cache feeds evict idle and over-limit content (see [Cache feeds and policies](index.md#cache-policy)); a build after a long idle period starts cold.

## Measuring your own pipelines

1. Record a few runs of the job without the cache feed.
2. Enable the cache (`build-cache: "true"` in [`ant-setup-action`](../ci/github-actions.md), or the flags on the [buildx](buildx.md) and [sccache](sccache.md) pages).
3. Run once to populate the cache, then compare several warm runs.
4. For Rust, keep the `cache-stats` step with `if: always()` so each run's hits and misses are in the job summary; for Docker, count `CACHED` steps in the `--progress=plain` log.
