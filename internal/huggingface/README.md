# Hugging Face local adapter

Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/huggingface/` for feeds of format `huggingface`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/huggingface.md`. Clients use `HF_ENDPOINT=<origin>/feeds/{slug}/huggingface` with `HF_TOKEN`.
Credentials are verified by the shared client-authentication layer (Bearer or Basic).

Read: `GET|HEAD /api/{models|datasets}/{repo}[/revision/{rev}]`, `/tree/{rev}[/path]`, `/api/whoami-v2`,
`/{repo}/resolve/{rev}/{path}` and `/datasets/{repo}/resolve/{rev}/{path}` (ETag = sha256, X-Repo-Commit,
X-Linked-Etag/Size, single-range + If-Range, HEAD verifies the stored object).

Publish: `PUT /api/{type}/{repo}/ant-upload/{session}/{path}` (streamed to disk, optional `X-Checksum-Sha256`),
then `POST .../ant-commit/{session}` with `{"refs":["main"]}`. Commit id = sha1 of the sorted tree; files are
published first and a manifest asset last, so a revision is atomically visible. Refs are append-only versions of a
`refs/...` component (newest wins). `DELETE .../ant-ref/{name}` tombstones a ref (needs delete scope); commits are immutable.

Limits: repo ids are 1-2 segments; revision names cannot contain `/`; no Xet, no commit deletion;
spool files use the OS temp dir. No migration needed.

## Remote sources (pull-through)

The source origin is the hub endpoint (`https://huggingface.co`); the file CDN hosts the hub redirects to (`cdn-lfs`, `xethub`, ...) must be configured as redirect origins of the source. The source credential, if any, is attached to hub requests only (gated and private repositories); neither it nor the client's token reaches a CDN host. Files are cached per upstream commit, one asset per file path; a branch or tag is only a short-lived pointer, and the resolved commit is remembered as a ref so cached files stay reachable by branch name during an outage. `api/{models,datasets}/<repo>[/revision/<rev>]` passes the upstream document through with its commit validated, `tree` listings are built from upstream tree documents (recursive listings walk the directories, bounded), and `resolve` serves the file with the sha256 and size from the upstream tree entry (LFS oid): a miss streams to the client while it is staged and the sha256 is checked at EOF (a mismatch aborts the connection and caches nothing); a Range request on a miss is served from the store after a verified fill.
