# Roadmap ledger

Cycle definitions live in the accepted [RFC-0001 roadmap](../docs/rfc/0001-herald-v0.1-roadmap.md) and are immutable; this table is the progress ledger and the source of truth for "what's next" (the first row not marked Done).

## v0.1 (RFC-0001)

| tlc Cycle | RFC Cycle | Scope | Status |
|---|---|---|---|
| `walking-skeleton` | A | Schema, auth, management API, transactional ingest, unsigned delivery loop | Done (PR #1) |
| `signing-and-secrets` | B | Standard Webhooks signing, secret rotation | Not started |
| `retry-ladder-and-endpoint-health` | C | Retry policy, disabling, redelivery — **blocked on drover reliability core merging** | Not started |
| `egress-hardening` | D | SSRF dialer, timeouts, politeness | Not started |
| `visibility-and-idempotent-ingest` | E | Paginated listing APIs, idempotency keys | Not started |
| `operations` | F | Health, metrics, packaging | Not started |
| `load-characterization` | G | Published load numbers — **blocked on drover worker pools** | Not started |
| `bulk-replay-and-retention` | H | Range replay, pruning | Not started |
