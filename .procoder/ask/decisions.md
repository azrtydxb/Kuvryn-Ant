## Secret scanner flags api/generated.go embedded spec

- Allowlist api/generated.go in .gitleaks.toml
- Commit once, bypassing the gate
- Stop for manual review

Key: Secret scanner flags api/generated.go embedded spec
Answer: Allowlist api/generated.go in .gitleaks.toml (owner, 2026-10-18). The flagged lines are inside the oapi-codegen gzip+base64 swaggerSpec blob.

## typescript-eslint cannot support TypeScript 7

- Exempt .tsx lint only, keep gate blocking
- Gate to report mode
- Downgrade TypeScript

Key: typescript-eslint cannot support TypeScript 7
Answer: Exempt .tsx lint only, keep the gate blocking; tsc still type-checks (owner, 2026-10-18). Upstream: azrtydxb/procoder#324.
