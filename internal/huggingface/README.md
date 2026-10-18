# Hugging Face local adapter

Subset of F12. Mount at `/feeds/{slug}/huggingface/` (feed format `huggingface`); `HF_ENDPOINT=<origin>/feeds/{slug}/huggingface`.
Machine Bearer (or Basic password) tokens only.

Read: `GET|HEAD /api/{models|datasets}/{repo}[/revision/{rev}]`, `/tree/{rev}[/path]`, `/api/whoami-v2`,
`/{repo}/resolve/{rev}/{path}` and `/datasets/{repo}/resolve/{rev}/{path}` (ETag = sha256, X-Repo-Commit,
X-Linked-Etag/Size, single-range + If-Range, HEAD verifies the stored object).

Publish: `PUT /api/{type}/{repo}/ant-upload/{session}/{path}` (streamed to disk, optional `X-Checksum-Sha256`),
then `POST .../ant-commit/{session}` with `{"refs":["main"]}`. Commit id = sha1 of the sorted tree; files are
published first and a manifest asset last, so a revision is atomically visible. Refs are append-only versions of a
`refs/...` component (newest wins). `DELETE .../ant-ref/{name}` tombstones a ref (needs delete scope); commits are immutable.

Limits: repo ids are 1-2 segments; revision names cannot contain `/`; no Xet, no remote/gated upstream, no commit deletion;
spool files use the OS temp dir. No migration needed.
