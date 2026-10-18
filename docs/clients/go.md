# Go modules

Feed URL: `https://<host>/feeds/go/go` (GOPROXY protocol; module path appended).
Auth: a read-scoped service token as the password in `~/.netrc` (Go sends netrc credentials over HTTPS).

```
# ~/.netrc  (chmod 600)
machine ant.example.com login x password <TOKEN>
```

```
export GOPROXY=https://ant.example.com/feeds/go/go
export GOSUMDB=sum.golang.org   # verified through the proxy: /sumdb/sum.golang.org/ is mirrored
# Private CA: export SSL_CERT_FILE=/path/ca.pem
```

The proxy mirrors `sum.golang.org` (`/sumdb/sum.golang.org/{supported,latest,lookup,tile}`), so checksum verification stays on. For private modules set `GOPRIVATE`/`GONOSUMDB` and `GONOPROXY=none`.
Private modules are ingested with an admin PUT of a bundle (info/mod/zip) to `/feeds/go/go/<module>/@v/<version>`; `go` itself has no publish command.

GitHub Actions:

```yaml
- run: |
    printf 'machine ant.example.com login x password %s\n' "$ANT_TOKEN" > ~/.netrc && chmod 600 ~/.netrc
    go build ./...
  env:
    ANT_TOKEN: ${{ secrets.ANT_TOKEN }}
    GOPROXY: https://ant.example.com/feeds/go/go
```
