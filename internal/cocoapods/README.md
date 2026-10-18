# CocoaPods adapter (F04)

Mount at `/feeds/{slug}/cocoapods/`; feed format `cocoapods`. Use as
`source 'https://user:TOKEN@host/feeds/x/cocoapods/'` (Basic password = machine token).
Requires `SetPublicURL` (archive URLs are written into podspecs at publish time).

- CDN reads: `CocoaPods-version.yml` (prefix_lengths 1,1,1), `all_pods_versions_a_b_c.txt`,
  `Specs/a/b/c/<Pod>/<ver>/<Pod>.podspec.json`, `deprecated_podspecs.txt` (empty).
- Archives: `archives/<Pod>/<ver>/<Pod>-<ver>.tar.gz|zip`.
- Publish: `PUT pods/<Pod>/<ver>` multipart/form-data: `podspec` (JSON), `source`
  (tar.gz or zip), optional `sha256`. The podspec `source` is rewritten to
  `{http,type,sha256}` pointing at the feed; podspec+archive commit in one PublishBundle.
- Delete: `DELETE pods/<Pod>/<ver>` (needs Delete action) unlists the version
  (stored in `components.metadata.unlisted`; no migration). Bytes stay until GC;
  an unlisted version cannot be republished (409).
- Resolution: package-owner per pod name.

Limitations: no external/VCS-only source references (publish requires an archive);
podspec URLs embed the public URL in force at publish time; archive downloads need
client credentials; not exercised against the real CocoaPods 1.17.0 client.

Verify: `sh internal/cocoapods/test-postgres.sh`
