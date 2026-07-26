# Synthesis — Founding research fleet

**Date**: 2026-07-25 · **Inputs**: rq01–rq05 · **Output**: ADR-0001..0006, RFC-0001

## Completeness critique (run inline)

- **Convergence**: all five questions independently landed on the same product shape — an API-first sender whose differentiator is operational simplicity (Postgres-only, single binary, transactional enqueue) in a field where every competitor requires Redis and multiple service roles (rq03); Svix as the semantic reference for retries, disabling, and the entity vocabulary (rq01, rq05); Standard Webhooks as the signing scheme (rq02); delivery executed as drover jobs (rq04, rq05).
- **Tensions resolved**: (1) rq01's Svix-shaped fixed retry ladder vs rq04's lean on drover's default `attempt^4` backoff — resolved in favor of the ladder, because drover cycle B ships a pluggable `RetryPolicy` and herald installs its own schedule; drover supplies the machinery, herald supplies the policy. (2) rq04's ~10-attempt/7h sketch vs rq01's 8-attempt/~28h ladder — resolved for the 8-attempt ladder, which matches published provider practice and receiver expectations. (3) Circuit breakers (rq04) — excluded from v0.1; the endpoint-disabling policy plus durable backoff already occupy that niche with durable state.
- **Open question closed after the fleet**: rq04 flagged "does `Snooze` consume an attempt?" — confirmed against drover's cycle-B spec: a snooze restores `attempt` to its pre-claim value (floored at zero), so politeness snoozes never exhaust retry budget.
- **Deferred decisions** (tracked, not decided): per-endpoint rate limiters (until 429 pressure is observed); ed25519 `v1a` asymmetric signatures; event-type registry table; Postgres RLS (schema is RLS-ready); bulk time-range replay UX; retention/pruning policy values.
- **Biggest risk**: sequencing on drover. Herald's retry cycle requires drover cycle B (RetryPolicy, `Cancel`, `Snooze`, rescuer) merged, and throughput work requires drover cycle C (worker pools; the current loop is single-flight). If drover's roadmap slips, herald's cycles C+ slip with it.
- **Re-check triggers**: drover cycle B's merged `RetryPolicy` signature differing from the in-flight spec; Standard Webhooks spec releasing a v2; Svix changing its published retry/disable policy; drover cycle D's `ScheduledAt` landing (would enable job-per-attempt layouts ruled out today).

## Recommendations → promotion

| RQ | Recommendation (★) | Promoted to |
|---|---|---|
| rq01 | At-least-once, 8-attempt Svix-shaped ladder (~28h) with jitter, 2xx-only, 410 disables, 15s/5s timeouts, 5-day auto-disable, manual re-enable + single-message redelivery | ADR-0003 |
| rq02 | Standard Webhooks `v1` HMAC-SHA256, sign per attempt, ±5 min documented tolerance, 24h dual-signing rotation, dial-time SSRF policy | ADR-0004, ADR-0006 |
| rq03 | Scope cut: signing, retries, attempt logs, endpoint health, replay, per-endpoint politeness in; portal UI, transformations, non-HTTP destinations, static IPs, FIFO out | ADR-0001, RFC-0001 |
| rq04 | Dispatcher: per-attempt context deadline, tuned Transport, body capture/drain caps, per-endpoint semaphore + `Snooze`, no circuit breaker in v0.1 | ADR-0006 |
| rq05 | Tenant → application → endpoint entity set, UUIDv7, tenant-scoped composite indexes, hashed `hrld_` API keys; one drover job per (message, endpoint) with herald-owned `RetryPolicy` and transactional ingest via `InsertTx` | ADR-0002, ADR-0005 |
