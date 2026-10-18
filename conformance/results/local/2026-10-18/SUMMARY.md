# Conformance summary

Target: `local` Ant: `git:5ad3f59` Date: 2026-10-18

| Format | Result | Pass | Fail | Documented limitation | Skip | Clients                                           |
| ------ | ------ | ---- | ---- | --------------------- | ---- | ------------------------------------------------- |
| cargo  | PASS   | 8    | 0    | 1                     | 0    | cargo `cargo 1.99.1 (5f94df478 2026-08-27)`       |
| docker | FAIL   | 10   | 1    | 1                     | 1    | crane `8a72a424fdecb4caa14f2d525e5d2503331442b5`  |
| go     | FAIL   | 7    | 1    | 0                     | 1    | go `go version go1.27.2 linux/arm64`              |
| npm    | PASS   | 8    | 0    | 2                     | 0    | node `pnpm 10.18.3 / node v24.22.0 / npm 11.19.1` |

### cargo

- **unsupported** `delete/unlist: cargo yank 0.1.1`: documented limitation (internal/cargo README: yank returns policy denial): exit 101: Updating `ant` index Yank kcdemo-4b343b@0.1.1 error: failed to yank from the registry at http://127.0.0.1:61647/feeds/cf-cargo-local-4b343b/cargo Caused by: the remote server responded with an error (status 403 Forbidden): registry policy disables yank and unyank

### docker

- **unsupported** `delete: crane delete manifest by tag`: documented limitation (README: unsupported lifecycle operations fail explicitly): exit 1: sha256:9a205c3fdcca82823b703bd04c78b23f57c06b18ba73c8be2ef5460080bf8e2c 2026/10/18 17:18:10 logged in via /work/home/.docker/config.json Error: DELETE http://127.0.0.1:61647/v2/cf-docker-local-11fc38/demo-11fc38/manifests/v2: UNSUPPORTED: operation unavailable
- **fail** `remote pull-through miss via registry-1.docker.io: crane digest+pull of library/hello-world`: exit 1: 2026/10/18 17:18:14 logged in via /work/home/.docker/config.json 2026/10/18 17:18:14 retrying HEAD http://127.0.0.1:61647/v2/cf-docker-hub-11fc38/library/hello-world/manifests/latest: unexpected status code 503 Service Unavailable (HEAD responses have no body, use GET for details) 2026/10/18 17:18:16 retrying HEAD http://127.0.0.1:61647/v2/cf-docker-hub-11fc38/library/hello-world/manifests

### go

- **fail** `remote pull-through hit: fresh module cache download with upstream unreachable`: exit 1: go: github.com/google/uuid@v1.6.0: reading http://host.docker.internal:59862/feeds/cf-go-remote-d84df7/go/github.com/google/uuid/@v/v1.6.0.zip: 503 Service Unavailable server response: Go proxy unavailable

### npm

- **unsupported** `same-endpoint write then read: publish 0.1.1 with default (latest) tag`: documented limitation (internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts): exit 1: npm notice npm notice package: @kcdemo/pkg-dbc542@0.1.1 npm notice Tarball Contents npm notice 33B index.js npm notice 98B package.json npm notice Tarball Details npm notice name: @kcdemo/pkg-dbc542 npm notice version: 0.1.1 npm notice filename: kcdemo-pkg-dbc542-0.
- **unsupported** `delete/unlist: npm unpublish 0.1.2`: documented limitation (internal/npmregistry README: unpublish returns 501): exit 1: npm warn using --force Recommended protections disabled. npm error code E405 npm error 405 Method Not Allowed - PUT http://127.0.0.1:61647/feeds/cf-npm-local-dbc542/npm/@kcdemo%2fpkg-dbc542/-rev/undefined - Method Not Allowed npm error A complete log of this run can be found in: /work/home/.npm/_logs/2026-10-18T18_
