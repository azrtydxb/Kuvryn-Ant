# p2repo

Eclipse p2 adapter mounted at `/feeds/{slug}/p2/` (feed format `p2`).

- Read: `p2.index`, `content.{xml,jar,xml.xz}`, `artifacts.{xml,jar,xml.xz}`, `plugins/`, `features/`, `binary/`.
- Publish: `PUT /feeds/{slug}/p2/repository.zip` (admin token, Basic password or Bearer) with a zipped p2 repo
  (content.xml|jar|xml.xz, artifacts.xml|jar|xml.xz, artifact files). Every artifact's `download.checksum.sha-256`/`download.md5`/`download.size`
  is verified, IU artifact references must resolve (upload or existing), `format` (pack200) is rejected.
- Model: artifacts are immutable components `artifact/{classifier}/{id}`; a content-addressed `manifest` component commits the upload atomically.
  Indexes are merged from all manifests deterministically; the same IU/artifact id+version with different bytes returns 409.
- Limitations: no delete/retention yet, no composite import, IU `requires` closure not checked (only artifact closure).
- Test: `sh internal/p2repo/test-postgres.sh` from the repo root.

## Remote sources (pull-through)

The remote source origin is the p2 repository root (the directory holding `p2.index`, `content.*` and `artifacts.*`). Same trust model
as APT and RPM:

- `p2.index`, `content.{xml,jar,xml.xz}` and `artifacts.{xml,jar,xml.xz}` are served verbatim from upstream (metadata cache, with a
  streamed fallback above the cache limit), never rewritten or merged; a variant the upstream does not have is a 404. A repository
  is answered by one owner: a locally ingested snapshot is never extended from upstream.
- `plugins/`, `features/` and `binary/` files are served only if the upstream artifacts index lists them (xz, jar or xml, parsed
  leniently: packed `format` artifacts and artifacts without checksum are skipped, not fatal). The bytes are verified against
  `download.checksum.sha-256` (or `download.md5` plus `download.size`) before the first byte is served, then cached. Signed JAR
  bytes are never touched, so the director's own signature checks still apply.

Limits: the standard layout only (`plugins/`, `features/`, `binary/` with the mapping rules that the local adapter advertises);
composite repositories (`compositeContent.xml`) and Pack200 artifacts are not supported; indexes come from the in-process metadata
cache (no durable offline index; cached files remain downloadable).
