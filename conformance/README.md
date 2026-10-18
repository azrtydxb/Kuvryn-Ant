# Conformance harness

Runs real ecosystem clients, in pinned container images, against a Kuvryn Ant instance and records machine-readable evidence.

    conformance/run.sh <target> [formats...]      # target: local | oke | kw | any label

Environment: `ANT_BASE_URL` (oke and kw default to their URLs; local auto-starts a throwaway instance via `dev-instance.sh`), `ANT_TOKEN` or `ANT_TOKEN_FILE` (raw token, or credential JSON with a `secret`/`token` field), `ANT_CA_BUNDLE` (PEM for the private CA of kw), `ANT_VERSION` (label stored as the Ant version, default git sha), `CONF_CLIENT_TIMEOUT` (per client command, default 240s).

Each scenario (`harness/scenarios/<format>.py`) does: publish, consume, same-endpoint write then read, delete/unlist, auth denial, and remote pull-through miss then hit against a real public upstream (the hit step re-points the source at an unreachable origin, so only the local cache can serve). Steps that hit a documented adapter limitation are marked `unsupported`, not `fail`. Loopback targets are reached through a socat sidecar so clients can use `127.0.0.1:<port>` without TLS.

Output: `results/<target>/<date>/<format>.json` (clients and versions, steps with status, timings, output tail, Ant version) and `SUMMARY.md`. The token is passed to containers by environment variable and redacted from all recorded output; the runner asserts it is absent before writing.

Implemented: docker (crane), cargo, go, npm (pnpm). Client images are listed in `clients.json`. The workflow `.github/workflows/conformance.yml` is `workflow_dispatch` only.
