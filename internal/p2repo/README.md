# p2repo

Eclipse p2 adapter mounted at `/feeds/{slug}/p2/` (feed format `p2`).

- Read: `p2.index`, `content.{xml,jar,xml.xz}`, `artifacts.{xml,jar,xml.xz}`, `plugins/`, `features/`, `binary/`.
- Publish: `PUT /feeds/{slug}/p2/repository.zip` (admin token, Basic password or Bearer) with a zipped p2 repo
  (content.xml|jar|xml.xz, artifacts.xml|jar|xml.xz, artifact files). Every artifact's `download.checksum.sha-256`/`download.md5`/`download.size`
  is verified, IU artifact references must resolve (upload or existing), `format` (pack200) is rejected.
- Model: artifacts are immutable components `artifact/{classifier}/{id}`; a content-addressed `manifest` component commits the upload atomically.
  Indexes are merged from all manifests deterministically; the same IU/artifact id+version with different bytes returns 409.
- Limitations: no delete/retention yet, no remote/composite import, IU `requires` closure not checked (only artifact closure).
- Test: `sh internal/p2repo/test-postgres.sh` from the repo root.
