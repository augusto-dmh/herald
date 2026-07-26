# Herald Project Context

Herald is a self-hosted, multi-tenant webhook delivery service written in Go (module `github.com/augusto-dmh/herald`), built on the drover task-queue library (`github.com/augusto-dmh/drover`, same author). Applications POST messages to herald's HTTP API; herald signs them, delivers them to registered endpoints, retries failures on a documented schedule, disables endpoints that stay broken, and keeps a queryable log of every delivery attempt. Philosophy: a small, auditable core; Postgres as the only infrastructure dependency; every load-bearing decision recorded in an ADR grounded in dated research.

## Current Status

Cycle progress lives in `.specs/ROADMAP.md` — never duplicate status here. Founded 2026-07-25 (research fleet, ADR-0001..0006, RFC-0001).

## Working in This Repo (Operational)

- The `Makefile` is the single source of truth for verification: `make build`, `make gate-quick`, `make gate-full` (integration tests need Docker — probe `docker ps` first), `make lint`, `make vulncheck`. CI mirrors these. Reference target names in docs and agent briefs; never inline the underlying commands.
- Tool versions are pinned in the Makefile and CI together — when bumping one, bump both in the same commit.
- Bash cwd resets between calls: always use absolute paths.
- Never `sleep N && cmd` — use `gh pr checks <n> --watch` as a background task or a Monitor until-loop.
- Any AskUserQuestion must mark a recommended option and give why-recommend AND why-not for every option.
- On 2+ consecutive provider 5xx errors, check the status page before retrying; on a confirmed outage, park with one long wakeup instead of retry loops. These rules apply to subagents too — include them in delegated briefs.

## Durable Decisions

Never re-litigate these without a superseding ADR:

- **Scope** (ADR-0001): API-first sender, Postgres-only, single binary. Walls: no portal UI, no transformations, no non-HTTP destinations, no static egress IPs, no ordering guarantees.
- **Domain model** (ADR-0002): tenant → application → endpoint, Svix-shaped vocabulary; `tenant_id` columns with composite indexes (RLS-ready, deferred); UUIDv7 IDs; prefixed API keys hashed at rest with `full`/`ingest` scopes.
- **Delivery contract** (ADR-0003): at-least-once; fixed 8-attempt ladder (0s, 5s, 5m, 30m, 2h, 5h, 10h, 10h) ±10% jitter; 2xx-only success; redirects never followed; 410 disables immediately; 5-day auto-disable with manual re-enable; 15s/5s timeouts.
- **Signing** (ADR-0004): Standard Webhooks `v1` HMAC-SHA256, signed per attempt (stable `webhook-id`, fresh timestamp); ±5 min documented tolerance; 24h dual-signing rotation via `endpoint_secrets`.
- **Drover seam** (ADR-0005): one drover job per (message, endpoint); ingest commits message + deliveries + jobs in one `InsertTx` transaction; herald owns the `RetryPolicy`; the worker returns `Cancel` at the cap; the API reads herald projections, never `drover_jobs`.
- **Egress** (ADR-0006): SSRF blocked in the dialer's `Control` func (post-DNS, every dial); ports 80/443, HTTPS-only outside dev; per-attempt context deadline; response capture 4 KiB / drain cap 64 KiB; per-endpoint semaphore with snooze-on-contention; no circuit breaker in v0.1.

## Workflow

RESEARCH → RFC/ADR → tlc-spec-driven cycle → IMPLEMENT → PR

- Research fleets write to `docs/research/<YYYY-MM-DD>/` — one `rqNN-*.md` per question plus `synthesis.md`. ADRs cite research; never the reverse.
- One roadmap cycle = one PR = one tlc-spec-driven cycle. Cycle definitions live in the roadmap RFC under `docs/rfc/` (immutable once accepted); progress lives in `.specs/ROADMAP.md` (the ledger).
- "Ship the next PR" → `herald-ship-cycle` (check `.specs/.ship-status` first). All commit/branch/PR publishing goes through `herald-finalize`. Explicit "review PR #N" → `pr-review`.
- History is self-contained: no internal IDs (task, phase, cycle, requirement), no `.specs/` paths, and no AI attribution in commit messages, PR titles, or PR bodies.
- Every behavior change ships with tests derived from spec acceptance criteria. `-race` always. Integration tests live behind the `integration` build tag and use testcontainers.

## Progressive Documentation Loading

Only read documents relevant to the current task — do not load all project documentation at once:

- What's next on the roadmap → `.specs/ROADMAP.md`, then the roadmap RFC in `docs/rfc/`
- Why a decision was made → `docs/adr/`
- Evidence behind a decision → `docs/research/` (only when re-opening a decision)
- Cycle working state → `.specs/features/<cycle>/` and `.specs/STATE.md`

## Current Constraints

- Solo maintainer: cycles must stay PR-sized.
- Public repo: technical merit only — no personal-motivation framing in committed files.
- Drover is pre-v0.1: the roadmap must not depend on unmerged drover cycles without flagging the sequencing constraint explicitly in the RFC.
