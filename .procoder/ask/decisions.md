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

## Runner language mirrors before client auth lands

- Make go/npm/cargo mirror feeds anonymous now and point runners at them
- Wait for client auth (blocked on procoder#324)

## Phase 2 D1 federation transport

- Dedicated mTLS listener (own Service/port)
- Signed requests through existing ingress

## Phase 2 D2 federation key custody

- Offline recovery key held by owner; signing keys encrypted on authority instance
- All keys online on the authority instance

## Phase 2 D3 federation control plane

- One designated authority instance with explicit handover
- Consensus cluster

## Phase 2 D4 stale policy behavior

- Keep enforcing last good policy and alert (optional hard-expiry opt-in)
- Hard expiry by default

## Phase 2 D5 routing default

- Proxy
- Redirect

## Phase 2 D6 federated writes

- One home instance per feed with planned/forced transfer
- Publish forwarding to the home instance

## Idempotency for secret-returning client-auth endpoints

- PAT create: replay with same key returns 409 (secret never stored); GitHub OIDC exchange exempt (auth, not a mutation); land now, convert remaining mutations in a follow-up
- Exempt PAT create and the exchange from idempotency entirely
- Hold the branch until every client-auth mutation is idempotent

Key: Runner language mirrors before client auth lands
Answer: Wait for client auth (owner, 2026-10-18). Only the Docker mirror stays live for runners.

Key: Phase 2 D1 federation transport
Answer: Dedicated mTLS listener (owner, 2026-10-18)

Key: Phase 2 D2 federation key custody
Answer: Offline recovery key held by owner; signing keys encrypted on authority instance (owner, 2026-10-18)

Key: Phase 2 D3 federation control plane
Answer: One designated authority instance with explicit handover (owner, 2026-10-18)

Key: Phase 2 D4 stale policy behavior
Answer: Keep enforcing last good policy and alert (owner, 2026-10-18)

Key: Phase 2 D5 routing default
Answer: Proxy (owner, 2026-10-18)

Key: Phase 2 D6 federated writes
Answer: One home instance per feed with planned/forced transfer (owner, 2026-10-18)

Key: Idempotency for secret-returning client-auth endpoints
Answer: PAT create replay returns 409 (secret never stored); GitHub OIDC exchange exempt; merge client auth now; remaining client-auth mutations get Idempotency-Key in a follow-up (owner, 2026-10-19).
