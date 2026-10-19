# Questions procoder cannot answer for you

Written 2026-10-19 05:09 UTC.

Answer each one by writing a line beginning `Answer: ` under it, then
hand the file back with `procoder ask --file .procoder/ask/QA.md`.
Leave the `Key:` lines alone — they are what ties an answer to its question.

## Q1: [decision] decisions.md

Key: 700d744aa4b5
Question: Idempotency for secret-returning client-auth endpoints

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

Answer:

## Q2: [decision] decisions.md

Key: eb142b64e69b
Question: Phase 2 D1 federation transport

- Dedicated mTLS listener (own Service/port)
- Signed requests through existing ingress

Answer:

## Q3: [decision] decisions.md

Key: daa364c9687e
Question: Phase 2 D2 federation key custody

- Offline recovery key held by owner; signing keys encrypted on authority instance
- All keys online on the authority instance

Answer:

## Q4: [decision] decisions.md

Key: da0c8ea964f4
Question: Phase 2 D3 federation control plane

- One designated authority instance with explicit handover
- Consensus cluster

Answer:

## Q5: [decision] decisions.md

Key: 38fe92f07694
Question: Phase 2 D4 stale policy behavior

- Keep enforcing last good policy and alert (optional hard-expiry opt-in)
- Hard expiry by default

Answer:

## Q6: [decision] decisions.md

Key: b47e88d65bd9
Question: Phase 2 D5 routing default

- Proxy
- Redirect

Answer:

## Q7: [decision] decisions.md

Key: f5aeb37cb707
Question: Phase 2 D6 federated writes

- One home instance per feed with planned/forced transfer
- Publish forwarding to the home instance

Answer:

## Q8: [decision] decisions.md

Key: 8358cb94ae39
Question: Runner language mirrors before client auth lands

- Make go/npm/cargo mirror feeds anonymous now and point runners at them
- Wait for client auth (blocked on procoder#324)

Answer:

## Q9: [decision] decisions.md

Key: 694bb2f1300c
Question: Secret scanner flags api/generated.go embedded spec

- Allowlist api/generated.go in .gitleaks.toml
- Commit once, bypassing the gate
- Stop for manual review

Key: Secret scanner flags api/generated.go embedded spec
Answer: Allowlist api/generated.go in .gitleaks.toml (owner, 2026-10-18). The flagged lines are inside the oapi-codegen gzip+base64 swaggerSpec blob.

Answer:

## Q10: [decision] decisions.md

Key: f7c7b86f553a
Question: typescript-eslint cannot support TypeScript 7

- Exempt .tsx lint only, keep gate blocking
- Gate to report mode
- Downgrade TypeScript

Key: typescript-eslint cannot support TypeScript 7
Answer: Exempt .tsx lint only, keep the gate blocking; tsc still type-checks (owner, 2026-10-18). Upstream: azrtydxb/procoder#324.

Answer:
