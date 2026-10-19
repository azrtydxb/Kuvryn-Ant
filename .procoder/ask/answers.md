# What a human decided

Written 2026-10-19 05:10 UTC. procoder reads this
file to avoid asking a question twice; edit an answer here to change what
it believes. Reword the question and it will be asked again.

## (no longer asked)

Key: Idempotency for secret-returning client-auth endpoints

Answer: PAT create replay returns 409 (secret never stored); GitHub OIDC exchange exempt; merge client auth now; remaining client-auth mutations get Idempotency-Key in a follow-up (owner, 2026-10-19).

## (no longer asked)

Key: Phase 2 D1 federation transport

Answer: Dedicated mTLS listener (owner, 2026-10-18)

## (no longer asked)

Key: Phase 2 D2 federation key custody

Answer: Offline recovery key held by owner; signing keys encrypted on authority instance (owner, 2026-10-18)

## (no longer asked)

Key: Phase 2 D3 federation control plane

Answer: One designated authority instance with explicit handover (owner, 2026-10-18)

## (no longer asked)

Key: Phase 2 D4 stale policy behavior

Answer: Keep enforcing last good policy and alert (owner, 2026-10-18)

## (no longer asked)

Key: Phase 2 D5 routing default

Answer: Proxy (owner, 2026-10-18)

## (no longer asked)

Key: Phase 2 D6 federated writes

Answer: One home instance per feed with planned/forced transfer (owner, 2026-10-18)

## (no longer asked)

Key: Runner language mirrors before client auth lands

Answer: Wait for client auth (owner, 2026-10-18). Only the Docker mirror stays live for runners.

## (no longer asked)

Key: Secret scanner flags api/generated.go embedded spec

Answer: Allowlist api/generated.go in .gitleaks.toml (owner, 2026-10-18). The flagged lines are inside the oapi-codegen gzip+base64 swaggerSpec blob.

## (no longer asked)

Key: typescript-eslint cannot support TypeScript 7

Answer: Exempt .tsx lint only, keep the gate blocking; tsc still type-checks (owner, 2026-10-18). Upstream: azrtydxb/procoder#324.
