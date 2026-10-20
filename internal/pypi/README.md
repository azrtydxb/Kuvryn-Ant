# PyPI adapter

`New(db *pgxpool.Pool, service *artifact.Service) (*Repository, error)` returns an HTTP handler for `/feeds/{slug}/pypi/`. It is mounted by `cmd/ant/artifacts.go` (see `site/formats/pypi.md`). The instance is pinned from `service.Lease().InstanceID`; foreign-instance credentials are rejected.

Implemented routes:

- `POST /feeds/{slug}/pypi/` (also `legacy/`): Twine legacy multipart `file_upload`, one wheel or `.tar.gz` sdist per request. Basic username plus service-token password translates to the shared Bearer authenticator. Explicit Bearer credentials also work; browser context alone does not.
- `GET`/`HEAD .../simple/` and `.../simple/{normalized-project}/`: HTML and PEP 691 JSON, content negotiation, SHA-256 links, Requires-Python and preserved yanked values. Names collapse case and runs of `-_.`; noncanonical project URLs redirect.
- `GET`/`HEAD .../files/{project}/{source}/{distribution-sha256}/{filename}`: authenticated source-bound downloads; append `.metadata` for verified core metadata. Wheel sidecars are advertised using PEP 714 core-metadata. Sdist metadata is retained but not advertised as build-invariant metadata.

URLs are origin-relative as permitted by the Simple API. Neither request Host nor TLS state constructs an origin; no public-origin configuration is needed. Indexes select one package owner in the handler instance, authorize before emitting metadata, and recheck source/digests through shared Lookup/Open. Missing versions never fall back to another source. Root indexes filter inaccessible projects. Responses are private/no-cache and vary by authorization and index representation.

Each filename has an internal version key `file:<sha256(filename)>`. Its distribution, exact core metadata, and adapter index metadata publish through one immutable `PublishBundle`, with references tying index metadata to both assets. This intentionally allows separately uploaded wheels/sdists for one Python release. It does **not** declare the entire release complete after a single file upload. Reusing a filename with changed bytes conflicts; identical retries replay. Prepare checks and pins all three asset destinations before accepting distribution bytes.

Validation never extracts archives or runs uploaded code. It verifies project/version agreement, filename shape, ZIP CRCs, wheel RECORD SHA-256/384/512 and sizes, WHEEL 1.0 tags, and required metadata files. It rejects unsafe paths, duplicate paths, links/special files, conflicting dist-info directories, bad checksums, and malformed metadata. Accepted filenames use the normalized PEP 440 spelling subset recognized in `metadata.go`; arbitrary legacy version spellings, legacy ZIP sdists and ZIP64 are not supported.

Resource bounds: concurrent uploads bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32); disk spooling with 64 KiB streaming; context checks every 64 KiB during expansion; 10,000 archive entries; 1 MiB per metadata file; 16 MiB ZIP central directory; expanded archive ceiling 512 MiB and expansion ratio 200 plus 1 MiB slack. Multipart fields are bounded to 64 KiB and 128 parts. Indexes have a 15-second context, 10,000 entry and 16 MiB aggregate/output ceilings. Database rows close before storage operations (tested with a one-connection pool). Request read/write deadlines are reset to 30-second progress windows, overriding short control-plane deadlines.

## Evidence and remaining integration work

Run `go test ./internal/pypi` for unit tests. With an isolated migrated PostgreSQL fixture, run:

```sh
ANT_TEST_DATABASE_URL='host=... user=... dbname=... sslmode=disable' \
ANT_REQUIRE_INTEGRATION=1 go test -race -v -count=1 ./internal/pypi
```

Tests cover archive safety, cancellation, RECORD integrity, normalized names, negotiation, atomic per-file publication, separate wheel/sdist uploads, replay/conflict, digest rejection, source binding, owner-only indexes, scoped metadata visibility, read-only publication denial, browser-context denial and instance binding. The native fixture runs installed `python3 -m pip`, downloads and installs a hash-pinned wheel, and rejects Python 3.7 against Requires-Python >=3.8. It uses one local httptest endpoint with no external index fallback or dependency fetching. Fixtures are generated in tests, with the resulting wheel digest logged.

The integration tests run installed `pip` against the adapter with a hash-pinned wheel fixture and reject a Python version outside `Requires-Python`. The real `pip` and `twine` clients (upload, install, pull-through of pypi.org) are exercised by `conformance/run.sh local pypi`.

Yank and unyank (`PUT` and `DELETE` on `files/{project}/{filename}/yanked`) and file delete (`DELETE files/{project}/{filename}`, gated by the feed's delete policy) are adapter API; no Python client has these commands.

## Remote sources (pull-through)

The project page is fetched from the remote source (PEP 691 JSON preferred, PEP 503 HTML accepted), reduced to the files the adapter can serve safely and re-rendered in the representation the client negotiated with every URL rewritten to this feed. File links embed the serving remote's source id and the file's sha256, exactly like locally published files. Upstream file URLs (often on a separate CDN origin, which must be a configured redirect origin) are looked up server-side when a file is requested; clients never supply an upstream URL.

Official references used:

- [Simple Repository API, HTML/JSON, hashes and metadata](https://packaging.python.org/en/latest/specifications/simple-repository-api/)
- [PyPI legacy upload API](https://docs.pypi.org/api/upload/)
- [Wheel format](https://packaging.python.org/en/latest/specifications/binary-distribution-format/)
- [Source distribution format](https://packaging.python.org/en/latest/specifications/source-distribution-format/)
- [Core metadata](https://packaging.python.org/en/latest/specifications/core-metadata/)
