# Maven release repository

`New(db, service) (*Repository, error)` returns a handler for
`/feeds/{slug}/maven/`. Mount it without stripping that prefix. The handler is
bound to `service.Lease().InstanceID`. No new Go dependencies, migrations, public
origin setting, or absolute URLs are needed. Parent integration must register the
handler and retain machine-credential authentication at this route.

Implemented: immutable local release bundles, Maven2 POM/JAR/classifier paths,
MD5/SHA-1/SHA-256 sidecars, package metadata, Basic (service token as password) and
Bearer service authentication, conditional GET/HEAD and byte ranges. Browser
session context alone is rejected. Private responses use `private, no-store`.

## Publication

POST `org/example/demo/1.0/_bundle` relative to the repository root with
`multipart/form-data`. Each part's literal `filename` is a Maven asset filename:
`demo-1.0.pom`, `demo-1.0.jar`, or `demo-1.0-{classifier}.jar`. Part field names
are ignored. Include an explicit-coordinate POM with `modelVersion` 4.0.0 and
packaging `jar` (default) or `pom`. JAR packaging requires the main JAR; POM
packaging requires the POM alone. A bundle is the explicit completion signal.

Example (service token supplied through the environment):

```sh
curl --fail --user "service:$ANT_TOKEN" \
  -F 'asset=@demo-1.0.pom' -F 'asset=@demo-1.0.jar' \
  https://repository.example/feeds/releases/maven/org/example/demo/1.0/_bundle
```

Up to four input assets become at most sixteen shared-artifact assets after
checksum generation. Optional per-part `X-Checksum-Sha256`, `X-Checksum-Sha1`,
and `X-Checksum-Md5` headers must match. Checksum files sent with a bundle are not accepted; with native PUT deploy they are verified and ignored. Legacy checksums provide client compatibility, not additional trust.
The service atomically commits the entire version, checksums, audit and outbox
records. Identical retries return 204; new bundles return 201; changed or expanded
existing versions return 409. Interrupted, malformed and incomplete bundles do
not advertise a version. Source routing is pinned before reading each asset and
rechecked by the shared service at commit.

Native `mvn deploy` works (Gradle maven-publish sends the same sequence but has not been run): each file is PUT separately
(jar, pom, their `.sha1`/`.md5`/`.sha256`/`.sha512` files, then the artifact level
`maven-metadata.xml`). PUT files are staged privately (validated like bundle parts,
checksum files verified, 15 minute TTL, at most 4 files per version and 16 versions
in flight) and the PUT of `{group}/{artifact}/maven-metadata.xml` is the completion
signal that publishes the version atomically; the uploaded metadata document is
discarded because the service generates its own. A deploy without the POM (or without
the jar for jar packaging) is rejected with 400 and never advertised, a changed
re-deploy of an existing version is 409, an identical one 204. Staged files are persisted in
PostgreSQL (`maven_deploy_*`, chunked, 15 minute TTL purged by the next deploy request), so the
PUTs of one deployment may reach different replicas and survive a restart; the metadata PUT
leases the staged sets so two replicas never commit one deployment twice. Timestamped snapshots and
SNAPSHOT metadata return 501; mutable snapshot generation support is not claimed.
Only releases are supported. Inherited/interpolated POM coordinates, packaging
other than jar/pom, group plugin-prefix metadata, signatures and remote proxy
fetching are outside this implementation. Gradle can consume the Maven layout,
but a native Gradle fixture has not been run.

## Bounds and ownership

Four concurrent uploads per handler; 256 MiB HTTP body; 128 MiB per JAR; 1 MiB
POM; 64 KiB streaming buffers. Temporary files are private and removed on every
exit. XML DTDs/entities are rejected, depth is capped at 64, and coordinates must
match the path. JAR entries are CRC-checked with context cancellation every
64 KiB, at most 10,000 entries, 1 GiB total expansion and a 1000:1 per-entry
expansion bound. Traversal, duplicate entries and symbolic links are rejected.
Transport deadlines are reset from the control API's overall deadline to 30-second
progress deadlines; the parent retains TLS/header limits.

Metadata uses one package owner and one instance, never alternate versions from
lower-priority sources. Rows are closed before policy and artifact calls (tested
with a one-connection pool). At most 10,000 versions are considered, metadata is
capped at 16 MiB and generated within a 15-second context. Latest/release identify
the most recently added release, with deterministic version tie-breaking.
Publication reads metadata from committed versions, avoiding metadata upload races.

## Verification

`go test ./internal/maven` runs unit tests; set `ANT_TEST_DATABASE_URL` to an
isolated PostgreSQL database to run integration tests. Set
`ANT_REQUIRE_INTEGRATION=1` to fail instead of skipping absent PostgreSQL.

The PG tests cover atomic completeness, checksums, immutable conflict/replay,
concurrent versions, package-owner non-fallback, namespace scope, foreign tokens,
browser-context rejection, ranges, Basic/Bearer and snapshot rejection. An
installed `mvn` runs a fresh-cache parent-POM resolution and `validate` through
a local authenticated HTTP fixture; all repositories are mirrored to the fixture
and no external artifacts/plugins are required. This exercises native Maven
model resolution, not native deploy, dependency JAR resolution or full format
conformance. Native Maven 3.9.16 and PostgreSQL integration passed with `-race`.

Protocol references:
- https://maven.apache.org/repositories/layout.html
- https://maven.apache.org/ref/3.9.9/maven-repository-metadata/repository-metadata.html
