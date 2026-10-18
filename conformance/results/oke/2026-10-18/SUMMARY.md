# Conformance summary

Target: `oke` Ant: `git:5ad3f59` Date: 2026-10-18

| Format | Result | Pass | Fail | Documented limitation | Skip | Clients                                           |
| ------ | ------ | ---- | ---- | --------------------- | ---- | ------------------------------------------------- |
| cargo  | PASS   | 8    | 0    | 1                     | 0    | cargo `cargo 1.99.1 (5f94df478 2026-08-27)`       |
| docker | FAIL   | 9    | 2    | 1                     | 1    | crane `8a72a424fdecb4caa14f2d525e5d2503331442b5`  |
| go     | PASS   | 8    | 0    | 0                     | 1    | go `go version go1.27.2 linux/arm64`              |
| npm    | PASS   | 8    | 0    | 2                     | 0    | node `pnpm 10.18.3 / node v24.22.0 / npm 11.19.1` |

### cargo

- **unsupported** `delete/unlist: cargo yank 0.1.1`: documented limitation (internal/cargo README: yank returns policy denial): exit 101: Updating `ant` index Yank kcdemo-710ca6@0.1.1 error: failed to yank from the registry at https://ant.azrty.com/feeds/cf-cargo-local-710ca6/cargo Caused by: the remote server responded with an error (status 403 Forbidden): registry policy disables yank and unyank

### docker

- **fail** `auth denial: /v2/ without or with bad token returns 401`: GET /v2/ -> 200, want 401
- **unsupported** `delete: crane delete manifest by tag`: documented limitation (README: unsupported lifecycle operations fail explicitly): exit 1: sha256:b2d07a84198e743a10389eb4479174ba1592d7196351a0ddd181572c67dd9a26 2026/10/18 18:25:11 logged in via /work/home/.docker/config.json Error: DELETE https://ant.azrty.com/v2/cf-docker-local-710ca6/demo-710ca6/manifests/v2: UNSUPPORTED: operation unavailable
- **fail** `remote pull-through miss via registry-1.docker.io: crane digest+pull of library/hello-world`: exit 1: 2026/10/18 18:26:02 logged in via /work/home/.docker/config.json 2026/10/18 18:26:03 retrying HEAD https://ant.azrty.com/v2/cf-docker-hub-710ca6/library/hello-world/manifests/latest: unexpected status code 503 Service Unavailable (HEAD responses have no body, use GET for details) 2026/10/18 18:26:04 retrying HEAD https://ant.azrty.com/v2/cf-docker-hub-710ca6/library/hello-world/manifests/l

### npm

- **unsupported** `same-endpoint write then read: publish 0.1.1 with default (latest) tag`: documented limitation (internal/npmregistry README: initial publish tags immutable; a second version using latest conflicts): exit 1: npm notice npm notice package: @kcdemo/pkg-710ca6@0.1.1 npm notice Tarball Contents npm notice 33B index.js npm notice 98B package.json npm notice Tarball Details npm notice name: @kcdemo/pkg-710ca6 npm notice version: 0.1.1 npm notice filename: kcdemo-pkg-710ca6-0.
- **unsupported** `delete/unlist: npm unpublish 0.1.2`: documented limitation (internal/npmregistry README: unpublish returns 501): exit 1: npm warn using --force Recommended protections disabled. npm error code E405 npm error 405 Method Not Allowed - PUT https://ant.azrty.com/feeds/cf-npm-local-710ca6/npm/@kcdemo/pkg-710ca6/-rev/undefined - Method Not Allowed npm error A complete log of this run can be found in: /work/home/.npm/_logs/2026-10-18T18_28_
