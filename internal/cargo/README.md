# Cargo local sparse adapter

Integration: `cargo.New(db, artifactService) (*Registry, error)` implements
`http.Handler`. Mount `/feeds/{slug}/cargo/`; use feed/source format `cargo`.
No additional module dependencies or migrations are required. The caller supplies
its shared artifact service with writer lease, routing and disk/S3 storage.

Clients configure `sparse+https://HOST/feeds/SLUG/cargo/` and a Cargo token
credential provider. Raw Authorization tokens pass through shared Bearer
validation. Config, index and download routes require read authorization;
publication uses Prepare for both keys then PublishBundle. Each immutable version
contains `crate` and `index` assets. Index composition enumerates one package owner
and verifies that every shared Open still resolves to that source. Build metadata
is excluded from the version storage key to enforce Cargo version uniqueness.

Publication accepts the documented little-endian length-prefixed JSON/archive
wire format. JSON and Cargo.toml are limited to 1 MiB; the compressed archive uses
the protocol's uint32 length. Archive expansion is limited to 1000 times compressed
size and 100,000 entries. Files are never extracted; traversal, links, duplicate
paths, missing manifests and mismatched literal package name/version are rejected.
Manifest identity validation accepts normalized Cargo manifests, not inherited
workspace identity. Upload/download deadlines refresh after progress (30 seconds),
and transfer chunks are at most 64 KiB. Four uploads can be active per handler.

Current explicit restrictions: crate names use lowercase ASCII letters, digits
and hyphens, start with a letter and have at most 64 characters. Uppercase and
underscore aliases are rejected to prevent ambiguous ownership. This restriction
must be shown in client documentation; it is not full Cargo name compatibility.
Yank/unyank use the core unlist/relist: allowed for publishers of the crate (delete rights also suffice) unless the feed delete policy is `denied`; owner/search
operations and Git index transport are unsupported. No remote fetch, replication
certification or MinIO certification is claimed by this package. Index responses
are bounded to 10,000 versions and 16 MiB of metadata.

Verification uses isolated PostgreSQL schemas via ANT_TEST_DATABASE_URL (required
when ANT_REQUIRE_INTEGRATION=1). `go test -race ./internal/cargo` covers publication,
retries, conflict, denial, malformed archives, checksums, cache validators and
owner isolation. When installed, Cargo publishes a crate, fetches it into a
consumer and checks that consumer offline. Executed with Cargo 1.97.1; the F23
1.99.1 pinned client remains to be certified.

Protocol references:
- https://doc.rust-lang.org/cargo/reference/registry-web-api.html
- https://doc.rust-lang.org/cargo/reference/registry-index.html
