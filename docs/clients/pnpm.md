# pnpm / npm / yarn

Registry: `https://<host>/feeds/npm/npm/`. Tarball URLs in metadata are rewritten to the feed; integrity is passed through from upstream.

```ini
# .npmrc
registry=https://ant.example.com/feeds/npm/npm/
//ant.example.com/feeds/npm/npm/:_authToken=${ANT_TOKEN}
# private CA: cafile=/path/ca.pem  (or NODE_EXTRA_CA_CERTS)
```

Publish a scoped package: `pnpm publish --no-git-checks` (or `npm publish`); it lands in the feed's default local source.
Known gap: `npm dist-tag add/rm` and unpublish return 501; `latest` is set on publish.

Yarn berry (`.yarnrc.yml`):

```yaml
npmRegistryServer: "https://ant.example.com/feeds/npm/npm/"
npmAuthToken: "${ANT_TOKEN}"
```

GitHub Actions: `env: ANT_TOKEN: ${{ secrets.ANT_TOKEN }}` with the `.npmrc` above checked in.
