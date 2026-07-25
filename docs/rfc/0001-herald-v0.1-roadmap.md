# RFC-0001: Herald v0.1 roadmap

- **Status**: Accepted (2026-07-25)
- **Driver**: Augusto de Melo Henriques
- **Approvers**: Augusto de Melo Henriques
- **Impact**: whole project — defines cycle order, the v0.1.0 cut line, and out-of-scope walls

## Background

The founding research fleet (`docs/research/2026-07-25/`, rq01–rq05 + synthesis) settled the product shape in ADR-0001..0006: an API-first, Postgres-only webhook delivery service built on drover, signing with Standard Webhooks, retrying on a fixed 8-attempt ladder, with SSRF-safe bounded egress. Each cycle below is one tlc-spec-driven cycle producing one PR. This RFC is immutable once accepted; progress is recorded in `.specs/ROADMAP.md`, never here.

## Locked decisions

- Cycles A–F constitute v0.1.0. G–H are visible-roadmap upside, droppable in that order.
- The ADR-0001 walls stand for the life of this RFC: no portal UI, no payload transformations, no non-HTTP destinations, no static egress IPs, no ordering guarantees.
- Sequencing on drover is explicit: cycle C requires drover's reliability core (retry policy, cancel/snooze, rescuer) merged; cycle G requires drover's worker pools. If drover slips, these cycles slip — they are not to be worked around with herald-side schedulers.
- Benchmarks are published only with methodology (cycle G); no drive-by numbers.

## Cycles

| Cycle | Deliverable | Core concepts exercised | Done when |
|---|---|---|---|
| **A — Walking skeleton** | Full v0.1 schema migrations; API-key auth middleware; minimal tenant/application/endpoint management API; message ingest with transactional enqueue (message + deliveries + drover jobs in one transaction); delivery worker POSTing unsigned; `delivery_attempts` recording; CI green | HTTP service structure, transactional ingest, the drover seam | An e2e test proves a message POSTed to the API reaches a local receiver and its attempt is queryable through the API |
| **B — Signing & secrets** | Standard Webhooks `v1` headers signed per attempt; `endpoint_secrets` with 24h dual-signing rotation; authenticated secret retrieval | HMAC construction, secret lifecycle | Deliveries verify with the standard-webhooks reference library, including throughout a rotation overlap |
| **C — Retry ladder & endpoint health** | Herald `RetryPolicy` implementing the 8-attempt ladder with jitter; attempt cap via `Cancel`; 410 immediate disable; 5-day auto-disable with hysteresis; manual re-enable; single-message redelivery | Retry scheduling atop drover, failure classification | A failing endpoint walks the full ladder to terminal state under compressed test clocks; 410, auto-disable, re-enable, and redelivery each proven by an integration test |
| **D — Egress hardening** | Dial-time SSRF policy (`Control` func); timeout taxonomy per ADR-0006; response capture/drain caps; per-endpoint semaphore with snooze-on-contention; HTTPS-only with dev flag | Custom dialers, bounded concurrency, politeness vs capacity | Integration tests show loopback/private/metadata/rebinding/redirect egress all blocked, and a stalled endpoint not delaying other endpoints' deliveries |
| **E — Visibility & idempotent ingest** | Keyset-paginated listing of messages/deliveries/attempts with filters; ingest idempotency keys | Cursor pagination on UUIDv7, idempotent APIs | Pagination is stable under concurrent inserts; re-POSTing an idempotency key returns the original message and enqueues nothing |
| **F — Operations** | `healthz`/`readyz`; Prometheus delivery metrics; structured `slog` throughout; single-binary `serve` command; example docker compose | Observability, service packaging | `docker compose up --wait` goes healthy and a metrics scrape shows delivery counters moving under a smoke test |
| **G — Load characterization** | Load harness; published throughput/latency numbers with pprof profiles and methodology in `docs/` | Measurement, profiling, backpressure under load | A committed methodology doc reports sustained load results with profiles, reproducible from the harness |
| **H — Bulk replay & retention** | Time-range bulk replay; attempt/message retention pruning | Batch enqueue, data lifecycle | Replaying a range creates exactly one new job per affected delivery, and pruning respects the documented retention policy |

## Out of scope (walls, not gaps)

Consumer portal UI · payload transformations · non-HTTP destinations · static egress IPs · FIFO/ordered delivery · event-type registry table · per-endpoint rate limiters (until 429 pressure is observed) · ed25519 `v1a` signatures · Postgres RLS
