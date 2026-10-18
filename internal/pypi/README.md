# PyPI adapter

`New(db *pgxpool.Pool, service *artifact.Service) (*Repository, error)` returns an HTTP handler for `/feeds/{slug}/pypi/`. Mount this route in the parent service. No additional Go dependencies or migrations are required. The instance is pinned from `service.Lease().InstanceID`; foreign-instance credentials are rejected. The parent owns router/storage configuration and HTTP integration.

Implemented routes:

- `POST /feeds/{slug}/pypi/` (also `legacy/`): Twine legacy multipart `file_upload`, one wheel or `.tar.gz` sdist per request. Basic username plus service-token password translates to the shared Bearer authenticator. Explicit Bearer credentials also work; browser context alone does not.
- `GET`/`HEAD .../simple/` and `.../simple/{normalized-project}/`: HTML and PEP 691 JSON, content negotiation, SHA-256 links, Requires-Python and preserved yanked values. Names collapse case and runs of `-_.`; noncanonical project URLs redirect.
- `GET`/`HEAD .../files/{project}/{source}/{distribution-sha256}/{filename}`: authenticated source-bound downloads; append `.metadata` for verified core metadata. Wheel sidecars are advertised using PEP 714 core-metadata. Sdist metadata is retained but not advertised as build-invariant metadata.

URLs are origin-relative as permitted by the Simple API. Neither request Host nor TLS state constructs an origin; no public-origin configuration is needed. Indexes select one package owner in the handler instance, authorize before emitting metadata, and recheck source/digests through shared Lookup/Open. Missing versions never fall back to another source. Root indexes filter inaccessible projects. Responses are private/no-cache and vary by authorization and index representation.

Each filename has an internal version key `file:<sha256(filename)>`. Its distribution, exact core metadata, and adapter index metadata publish through one immutable `PublishBundle`, with references tying index metadata to both assets. This intentionally allows separately uploaded wheels/sdists for one Python release. It does **not** declare the entire release complete after a single file upload. Reusing a filename with changed bytes conflicts; identical retries replay. Prepare checks and pins all three asset destinations before accepting distribution bytes.

Validation never extracts archives or runs uploaded code. It verifies project/version agreement, filename shape, ZIP CRCs, wheel RECORD SHA-256/384/512 and sizes, WHEEL 1.0 tags, and required metadata files. It rejects unsafe paths, duplicate paths, links/special files, conflicting dist-info directories, bad checksums, and malformed metadata. Accepted filenames use the normalized PEP 440 spelling subset recognized in `metadata.go`; arbitrary legacy version spellings, legacy ZIP sdists and ZIP64 are not supported.

Resource bounds: four concurrent uploads; disk spooling with 64 KiB streaming; context checks every 64 KiB during expansion; 10,000 archive entries; 1 MiB per metadata file; 16 MiB ZIP central directory; expanded archive ceiling 512 MiB and expansion ratio 200 plus 1 MiB slack. Multipart fields are bounded to 64 KiB and 128 parts. Indexes have a 15-second context, 10,000 entry and 16 MiB aggregate/output ceilings. Database rows close before storage operations (tested with a one-connection pool). Request read/write deadlines are reset to 30-second progress windows, overriding short control-plane deadlines.

## Evidence and remaining integration work

Run `go test ./internal/pypi` for unit tests. With an isolated migrated PostgreSQL fixture, run:

```sh
ANT_TEST_DATABASE_URL='host=... user=... dbname=... sslmode=disable' \
ANT_REQUIRE_INTEGRATION=1 go test -race -v -count=1 ./internal/pypi
```

Tests cover archive safety, cancellation, RECORD integrity, normalized names, negotiation, atomic per-file publication, separate wheel/sdist uploads, replay/conflict, digest rejection, source binding, owner-only indexes, scoped metadata visibility, read-only publication denial, browser-context denial and instance binding. The native fixture runs installed `python3 -m pip`, downloads and installs a hash-pinned wheel, and rejects Python 3.7 against Requires-Python >=3.8. It uses one local httptest endpoint with no external index fallback or dependency fetching. Fixtures are generated in tests, with the resulting wheel digest logged.

Executed locally on 2026-10-18: all 11 tests passed under `-race`, with pip 26.2.1 / Python 3.14 against temporary PostgreSQL and disk storage (one DB connection). The deterministic native wheel fixture SHA-256 was `84df5f834ae5cb60fe35d03a2ff659e061da8b48bbf9551c76aa6426c4ab3fb4`. Twine is not installed; multipart protocol tests do not constitute a native Twine run. No full F19 conformance claim: remote hydration, replica seed/offline tests, mutable yank management, lifecycle/health/metrics wiring, MinIO, sdist build/install and installed-client binary digest locking remain parent/shared-work dependencies. Imported yanked values render in HTML/JSON; no yank mutation endpoint is advertised. Remote/replica assets can only be read when materialized in the shared owner/asset model; this adapter does not implement upstream networking or implicit public fallback.

Official references used:

- [Simple Repository API, HTML/JSON, hashes and metadata](https://packaging.python.org/en/latest/specifications/simple-repository-api/)
- [PyPI legacy upload API](https://docs.pypi.org/api/upload/)
- [Wheel format](https://packaging.python.org/en/latest/specifications/binary-distribution-format/)
- [Source distribution format](https://packaging.python.org/en/latest/specifications/source-distribution-format/)
- [Core metadata](https://packaging.python.org/en/latest/specifications/core-metadata/)
