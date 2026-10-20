# Conformance harness

Runs real ecosystem clients, in pinned container images, against a Kuvryn Ant instance and records machine-readable evidence. The harness is Go: package `conformance/harness` (core, runner, one scenario file per group of formats) driven by `cmd/conformance`.

    conformance/run.sh <target> [formats...]      # target: local | oke | kw | any label
    go run ./cmd/conformance <target> [formats...]  # same, against ANT_BASE_URL

Environment: `CONF_KEEP_FEEDS=1` skips the per-run feed cleanup (for debugging a failed step), `ANT_BASE_URL` (the base URL of the instance; the oke and kw targets read `ANT_OKE_URL` and `ANT_KW_URL` when it is unset, and fail with a usage error when none is given; local auto-starts a throwaway instance via `dev-instance.sh`), `ANT_TOKEN` or `ANT_TOKEN_FILE` (raw token, or credential JSON with a `secret`/`token` field), `ANT_CLIENT_TOKEN` (optional; the credential package clients present, for example a user PAT, while `ANT_TOKEN` stays the admin credential for the management API), `ANT_CA_BUNDLE` (PEM for the private CA of kw; mounted into every client container and exported as `SSL_CERT_FILE` and the usual per-tool variables), `ANT_VERSION` (label stored as the Ant version, default git sha), `CONF_CLIENT_TIMEOUT` (seconds per client command, default 900), `CONF_RESULTS_DIR` (default `conformance/results`), `ANT_CONF_STATE` (state directory of the local instance; use a unique one when several people or agents share a machine).

Each scenario creates per-run feeds through the management API and deletes them (`DELETE /api/v1/feeds/{id}?force=true`) when the format finishes; the outcome is stored as `feed_cleanup` in the results. Sources have no delete endpoint, so each run leaves its (feed-less) sources behind.

Each scenario does: publish (native client where the adapter supports one, otherwise the documented admin ingestion call), consume with the real client, same-endpoint write then read, delete/unlist per the F-contract (native command where one exists, the adapter route otherwise, plus the management API unlist/relist/delete), remote pull-through miss then hit against a real public upstream (the hit step re-points the source at an unreachable origin, so only the local cache can serve), and auth denial. Authentication-failure steps run last in each scenario because the instance rate limits failed authentication attempts per client address (HTTP 429 `authentication attempts limited`), which would otherwise poison later steps.

A step that hits a documented adapter limitation is `unsupported`, not `fail`. A step that cannot apply (no remote support, no pinned client available) is `skip` with the reason.

Output: `results/<target>/<date>/<format>.json` (clients and versions, steps with status, timings, output tail, Ant version) and `SUMMARY.md`. The token is passed to containers by environment variable and redacted from all recorded output; the runner refuses to write evidence that still contains it. Loopback targets are reached through a socat sidecar so clients can use `127.0.0.1:<port>` without TLS.

Each run deletes the feeds and sources it created (`cf-<format>-<tag>-<runid>`) when the format finishes; `CONF_KEEP_FEEDS=1` keeps them. When no formats are named, the `oke` target skips `huggingface` (OKE has no GPUs and Hugging Face is not configured there); `local` and `kw` run every format, and `conformance/run.sh oke huggingface` still runs it explicitly.

Formats: docker, oras (oci-artifacts), cargo, go, npm, pypi, maven, nuget, helm, raw, rubygems, composer, conan, conda, apk, apt, rpm, terraform, swift, cocoapods, huggingface, ansible, cran, pub, git-lfs, p2. Client images are listed in `harness/clients.json`. The p2 director (Tycho/Eclipse platform) is not pinned into the harness; p2 is exercised at the protocol level and the director run is documented as manual in the p2 scenario. The workflow `.github/workflows/conformance.yml` is `workflow_dispatch` only.
