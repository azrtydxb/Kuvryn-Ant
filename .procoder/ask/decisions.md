## Secret scanner flags api/generated.go embedded spec
- Allowlist api/generated.go in .gitleaks.toml
- Commit once, bypassing the gate
- Stop for manual review

Decided: allowlist api/generated.go (owner, 2026-10-18). The flagged lines are inside the oapi-codegen gzip+base64 swaggerSpec blob.
