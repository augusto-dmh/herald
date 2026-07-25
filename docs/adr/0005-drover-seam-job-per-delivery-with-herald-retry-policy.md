# ADR-0005: One drover job per delivery, with herald owning the retry policy

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: architecture, queue, drover

## Context and Problem Statement

Herald executes deliveries through drover, and the seam between them decides where retry scheduling, attempt history, and delivery state live — the single most structural choice in the system.

## Decision Drivers

- Transactional ingest is the flagship: message rows and delivery jobs must commit in one Postgres transaction (`InsertTx`).
- Drover's cycle B provides a pluggable `RetryPolicy` (absolute-time scheduling per job) plus `Cancel`/`Snooze` sentinels; duplicating that machinery in herald would be a parallel scheduler.
- The delivery API must never query drover's internal tables; drover is an implementation detail.
- A rescuer re-running a crashed attempt must be recorded truthfully, not papered over.

## Considered Options

**One job per (message, endpoint), herald-owned `RetryPolicy`** · **One job per attempt, herald schedules re-enqueues** · **Herald-owned state machine, drover as bare executor**

## Decision Outcome

Chosen: **One job per (message, endpoint), herald-owned `RetryPolicy`**, with:

- **Enqueue**: ingestion writes the message, its per-endpoint `deliveries` rows, and one drover job per delivery in a single transaction — an accepted message can never lack its jobs.
- **Retry scheduling**: herald installs a `RetryPolicy` implementing the ADR-0003 ladder with jitter; drover supplies the machinery (state transitions, leases, rescuer), herald supplies the policy.
- **Attempt cap**: the worker returns `Cancel` when the ladder is exhausted or the endpoint is disabled; `delivery_attempts` has no uniqueness on attempt number, so a rescued re-run of the same attempt is recorded as what it is.
- **Projection**: `deliveries` carries the API-facing status; the API never reads `drover_jobs`.
- **Sequencing constraints**: herald's retry cycle requires drover cycle B merged (hard blocker); meaningful throughput requires drover cycle C's worker pools (the current loop is single-flight).

### Positive Consequences

- Zero dual-write window — the exact failure mode every Redis/MQ-queued competitor accepts.
- One retry scheduler in the system; herald's ladder is policy code, not infrastructure.

### Negative Consequences

- Herald's roadmap is coupled to drover's; two upstream cycles gate herald features.
- Per-(message, endpoint) jobs mean a message fanned out to N endpoints costs N job rows up front.

## Links

- Evidence: `docs/research/2026-07-25/rq05-domain-model-and-drover-seam.md`
- Related: ADR-0001, ADR-0002, ADR-0003
