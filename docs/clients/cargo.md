# Cargo (sparse registry)

Feed index: `sparse+https://<host>/feeds/cargo/cargo/` (`config.json` advertises `auth-required: true`).

```toml
# ~/.cargo/config.toml
[source.crates-io]
replace-with = "ant"
[source.ant]
registry = "sparse+https://ant.example.com/feeds/cargo/cargo/"
[registries.ant]
index = "sparse+https://ant.example.com/feeds/cargo/cargo/"
[registry]
global-credential-providers = ["cargo:token"]
# private CA: [http] cainfo = "/path/ca.pem"
```

Token: `export CARGO_REGISTRIES_ANT_TOKEN=<TOKEN>` (raw token, no "Bearer").
Publish: `cargo publish --registry ant`. Consume with `dep = { version = "0.1", registry = "ant" }`.
`cargo yank` is disabled by registry policy (403).

GitHub Actions: set `CARGO_REGISTRIES_ANT_TOKEN: ${{ secrets.ANT_TOKEN }}` and write the config above.
