## Summary

<!-- What does this change and why? Link the issue: Fixes #123 -->

## Type of change

- [ ] Bug fix
- [ ] New feature or format capability
- [ ] Documentation
- [ ] Build, CI or dependencies
- [ ] Refactoring without behaviour change

## Verification

<!-- Commands you ran and their result. -->

- [ ] `go build ./... && go vet ./...`
- [ ] `go test -count=1 ./...` and, for DB code, `scripts/test-postgres.sh <packages>`
- [ ] `scripts/check-generated.sh` (API contract and generated clients match)
- [ ] `cd web && npm test && npm run build` and `internal/console/dist` rebuilt (console changes)
- [ ] `cd site && npm run docs:build` (documentation changes)

## Checklist

- [ ] Commits follow Conventional Commits and are signed off (`git commit -s`).
- [ ] New migrations are numbered contiguously after `main`.
- [ ] Generated files were regenerated, not hand-edited.
- [ ] Documentation (`site/`, adapter README) and `CHANGELOG.md` are updated for user-visible changes.
- [ ] No secrets, real hostnames, internal IP addresses or personal data in code, docs or tests.
