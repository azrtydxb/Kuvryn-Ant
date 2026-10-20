# Cargo local sparse adapter

Integration: `cargo.New(db, artifactService) (*Registry, error)` implements
`http.Handler`. Mounted by `cmd/ant/artifacts.go` at `/feeds/{slug}/cargo/` for feeds of format `cargo`. The external origin comes from `ANT_PUBLIC_URL` through `SetPublicURL`. Real-client runs are driven by `conformance/` (see `conformance/README.md`); the user-facing description is `site/formats/cargo.md`. The caller supplies its shared artifact service with writer lease, routing and disk/S3 storage.

Clients configure `sparse+https://HOST/feeds/SLUG/cargo/` and a Cargo token
credential provider. Raw Authorization tokens (the form `cargo` sends) are resolved by the shared client-authentication layer exactly like Bearer, so service tokens, personal access tokens and GitHub JWTs all work with the feed's policy, scopes and audit; a raw value is never a password login (see `site/formats/cargo.md`). Config, index and download routes require read authorization;
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
and transfer chunks are at most 64 KiB. Concurrent uploads are bounded by the per-format upload slots (`ANT_UPLOAD_SLOTS`; by default two per CPU, between 4 and 32).

Current explicit restrictions: crate names use lowercase ASCII letters, digits
and hyphens, start with a letter and have at most 64 characters. Uppercase and
underscore aliases are rejected to prevent ambiguous ownership. This restriction
must be shown in client documentation; it is not full Cargo name compatibility.
Yank/unyank use the core unlist/relist: allowed for publishers of the crate (delete rights also suffice) unless the feed delete policy is `denied`; owner/search
operations and Git index transport are unsupported. Index responses
are bounded to 10,000 versions and 16 MiB of metadata.

Remote sources: the upstream sparse index file of a crate is read through the metadata cache, validated line by line and served verbatim (index lines carry no registry URLs; the feed's own `config.json` points `dl` and `api` back at the feed). A crate archive is fetched on first download from the upstream `dl` template, verified against the sha256 `cksum` of its index line and cached, with the verified index line beside it so the owner's versions stay listable during an outage. Reads accept crates.io style names (case-insensitive, underscores); local publication keeps the stricter rule above.

Verification uses isolated PostgreSQL schemas via ANT_TEST_DATABASE_URL (required
when ANT_REQUIRE_INTEGRATION=1). `go test -race ./internal/cargo` covers publication,
retries, conflict, denial, malformed archives, checksums, cache validators and
owner isolation. The real Cargo client (1.99.1 in the conformance harness) publishes, yanks, consumes and pulls through crates.io: `conformance/run.sh local cargo`.

Protocol references:

- https://doc.rust-lang.org/cargo/reference/registry-web-api.html
- https://doc.rust-lang.org/cargo/reference/registry-index.html
