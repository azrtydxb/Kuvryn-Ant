"""F23 Rust/Cargo sparse registry with real cargo (publish, consume, yank, auth denial, crates.io pull-through)."""

NAME = "F23"
CLIENTS_USED = ["cargo"]
CRATES_IO = {
    "origin": "https://index.crates.io",
    "redirect": ["https://static.crates.io"],
}


def run(c):
    slug = c.feed("cargo", "local")
    crate = "kcdemo-%s" % c.run_id
    idx = "%s/feeds/%s/cargo/" % (c.env.client_base, slug)
    e = {
        "CARGO_REGISTRIES_ANT_CREDENTIAL_PROVIDER": "cargo:token",
        "CARGO_REGISTRIES_ANTR_CREDENTIAL_PROVIDER": "cargo:token",
        "CARGO_REGISTRIES_ANT_INDEX": "sparse+" + idx,
        "CARGO_REGISTRIES_ANT_TOKEN": c.env.token,
        "CARGO_NET_GIT_FETCH_WITH_CLI": "true",
        "CARGO_HOME": "/work/cargo-home",
        "CARGO_TARGET_DIR": "/work/target",
    }
    # TOKEN is injected by the harness; CARGO_REGISTRIES_ANT_TOKEN carries the same secret and is redacted in output.
    mk = (
        'mkdir -p pub && cd pub && printf \'[package]\\nname = "%s"\\nversion = "VERSION"\\nedition = "2021"\\n'
        'description = "Kuvryn Ant conformance fixture"\\nlicense = "MIT"\\npublish = ["ant"]\\n\' > Cargo.toml && mkdir -p src && '
        'echo "pub fn answer() -> u32 { 42 }" > src/lib.rs' % crate
    )
    consumer = (
        'rm -rf cons && mkdir -p cons/src && cd cons && printf \'[package]\\nname = "consumer"\\nversion = "0.1.0"\\nedition = "2021"\\n'
        '[dependencies]\\n%s = { version = "VERSION", registry = "ant" }\\n\' > Cargo.toml && '
        "echo 'fn main(){ println!(\"answer={}\", %s::answer()); }' > src/main.rs"
        % (crate, crate.replace("-", "_"))
    )

    c.step("client-version", lambda: c.version("cargo", "cargo --version"))
    c.step(
        "publish: cargo publish 0.1.0 to feed",
        lambda: c.client(
            "cargo",
            mk.replace("VERSION", "0.1.0")
            + " && cargo publish --registry ant --allow-dirty",
            e,
        ),
    )
    c.step(
        "consume: cargo run with dependency from feed (fresh CARGO_HOME)",
        lambda: c.client(
            "cargo",
            "rm -rf /work/cargo-home; "
            + consumer.replace("VERSION", "0.1.0")
            + " && cargo run --quiet | grep answer=42",
            e,
        ),
    )
    c.step(
        "same-endpoint write then read: publish 0.1.1 then consumer resolves it",
        lambda: c.client(
            "cargo",
            mk.replace("VERSION", "0.1.1")
            + " && cargo publish --registry ant --allow-dirty && cd /work && "
            + consumer.replace("VERSION", "^0.1")
            + ' && cargo update && cargo tree | grep "%s v0.1.1"' % crate,
            e,
        ),
    )
    c.step(
        "delete/unlist: cargo yank 0.1.1",
        lambda: c.client(
            "cargo", "cd pub && cargo yank --registry ant %s@0.1.1" % crate, e
        ),
        known_unsupported="internal/cargo README: yank returns policy denial",
    )
    c.step(
        "auth denial: invalid token rejected by cargo",
        lambda: c.client(
            "cargo",
            "rm -rf /work/cargo-home; cd /work/cons && cargo fetch",
            dict(e, CARGO_REGISTRIES_ANT_TOKEN="invalid-token"),
            expect_fail=True,
        ),
    )
    c.step(
        "auth denial: index config.json requires credentials (401)",
        lambda: c.http("GET", "/feeds/%s/cargo/config.json" % slug, 401, token=None),
    )

    rslug = c.feed("cargo", "remote", CRATES_IO)
    rsrc = c.last_source
    r = {
        "CARGO_REGISTRIES_ANTR_CREDENTIAL_PROVIDER": "cargo:token",
        "CARGO_REGISTRIES_ANTR_INDEX": "sparse+%s/feeds/%s/cargo/"
        % (c.env.client_base, rslug),
        "CARGO_REGISTRIES_ANTR_TOKEN": c.env.token,
        "CARGO_HOME": "/work/cargo-home-r",
    }
    rcons = (
        'rm -rf rcons && mkdir -p rcons/src && cd rcons && printf \'[package]\\nname = "rconsumer"\\nversion = "0.1.0"\\nedition = "2021"\\n'
        '[dependencies]\\nitoa = { version = "=1.0.11", registry = "antr" }\\n\' > Cargo.toml && '
        "echo 'fn main(){ println!(\"{}\", itoa::Buffer::new().format(7u8)); }' > src/main.rs"
    )
    ok = c.step(
        "remote pull-through miss: cargo fetch/build itoa from crates.io via feed",
        lambda: c.client("cargo", rcons + " && cargo run --quiet | grep -x 7", r),
    )

    def hit():
        c.set_remote_origin(rsrc, "https://127.0.0.1:9", [])
        return c.client(
            "cargo",
            "rm -rf /work/cargo-home-r2; cd rcons && rm -f Cargo.lock; cargo fetch",
            dict(r, CARGO_HOME="/work/cargo-home-r2"),
        )

    c.step(
        "remote pull-through hit: fresh CARGO_HOME fetch with upstream unreachable",
        hit,
        skip_reason=None if ok else "miss step failed",
    )
