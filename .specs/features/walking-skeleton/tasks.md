# Tasks — `walking-skeleton`

Contract per task: tests derive from the spec's acceptance criteria and assert spec outcomes; the gate is green before the task is done; one atomic commit per task; no internal IDs and no tooling attribution in commit messages.

Gate vocabulary: per-task = affected package (`go test -race ./internal/<pkg>/...`); phase boundary = `make gate-quick` (+ `make gate-full` when the phase adds integration tests; probe `docker ps` first); pre-publish = `make build && make gate-full && make lint`.

## Phase A — Domain, schema, and store · Opus

| # | Task | Requirements | Gate |
|---|---|---|---|
| A1 | Module deps (`pgx/v5`, `google/uuid`, `drover@main`, `testcontainers-go`) + `internal/herald` domain types and enums with validation helpers | SKEL-13 | `go build ./... && go vet ./...` |
| A2 | `internal/migrate`: embed.FS runner + `herald_migrations` version table; migration `0001` with the full v0.1 schema | SKEL-13 | `go test -race ./internal/migrate/...` (integration) |
| A3 | `internal/testdb` testcontainers helper (container, pool, herald+drover migrations applied) | — | shared by A2/A4 tests |
| A4 | `internal/store`: tenants/keys (create, lookup-by-hash), applications, endpoints (incl. matching query: enabled + filter_types), messages+deliveries Tx variants, attempts (insert, list by message), delivery status update | SKEL-02, SKEL-07 (store side), SKEL-08 | `go test -race ./internal/store/...` (integration) |

Phase gate: `make gate-quick` + `make gate-full`.

**Phase A: DONE** — commits `bc63d94` (A1), `541f0a4` (A3), `26eba2c` (A2), `fe873c4` (A4). Tests 0 → 36 (15 unit, 21 integration); `make gate-quick`/`gate-full`/`lint`/`build` all green. Sensors mutation-verified: tenant-scoped fan-out, NULL-filter semantics, disabled exclusion, caller-tx composition (`store_test.go`). Deviations recorded in the Phase A report (notable: commit order A1→A3→A2→A4 so every commit compiles under `-tags=integration`; testdb takes schema funcs as arguments; deps land in first-importing commit; store adds `EndpointByID`, defers delivery-by-id to Phase C).

## Phase B — HTTP API and auth · Opus

| # | Task | Requirements | Gate |
|---|---|---|---|
| B1 | `internal/httpapi` scaffolding: server constructor, routing, JSON error envelope, body cap middleware | SKEL-09 | `go test -race ./internal/httpapi/...` |
| B2 | Auth: bootstrap-token check for tenant create; Bearer key middleware (SHA-256 lookup, tenant+scope context); scope enforcement | SKEL-01..03, SKEL-06 | same |
| B3 | Handlers: tenant create (one-time key), application create, endpoint create, message detail (deliveries+attempts) | SKEL-04, SKEL-05, SKEL-12 | same |
| B4 | Ingest handler: validation, transactional message+deliveries+drover `InsertTx` fan-out, 202 | SKEL-07, SKEL-08, SKEL-09 | same (integration) |

Phase gate: `make gate-quick` + `make gate-full`.

**Phase B: DONE** — commits `36957b1` (B1), `c36a0a1` (B2), `10f83c9` (B3), `cb2d108` (B4). Tests 36 → 64 (25 unit, 39 integration); `make gate-quick`/`gate-full`/`lint` green. Sensors mutation-verified: scope enforcement, 404-not-403 existence rule, and the atomicity seam (`InsertTx`→`Insert` mutation caught by a trigger-injected mid-transaction failure asserting zero rows in `messages`/`deliveries`/`drover_jobs`). Deviations recorded in the Phase B report (notable: caller passed as handler argument instead of request context — unscoped handlers cannot compile; routes registered with their handlers; `disabled` not settable at endpoint create; `TouchAPIKey` deliberately uncalled; 409 added to the error table for duplicate uids).

## Phase C — Delivery worker and e2e · Opus

| # | Task | Requirements | Gate |
|---|---|---|---|
| C1 | `internal/deliver`: `Args`, `Worker` (POST, attempt recording, status transitions, snippet cap, 15s timeout) | SKEL-10, SKEL-11 | `go test -race ./internal/deliver/...` (integration, httptest receivers) |
| C2 | e2e integration test: bootstrap → app → endpoint (local receiver) → ingest → drover loop delivers → GET shows delivered attempt; plus failure-path e2e (receiver 500 → `failed`) | SKEL-01..12, I-WS-1..4 | `make gate-full` |

Phase gate: pre-publish gate.

**Phase C: DONE** — commits `a746bf6` (C1), `5a5de9f` (C2). Tests 64 → 77 (31 unit, 46 integration); pre-publish gate green (`make build` / `make gate-full` / `make lint`). Sensors mutation-verified: 2xx-only boundary, attempt-row-on-failure, and error-propagation-to-queue (e2e asserts the failed job lands `dead` in `drover_jobs`). Deviations recorded in the Phase C report (notable: store gains the one unscoped-by-necessity `DeliveryWorkForJob` read; redirects not followed per the delivery-contract ADR; configurable timeout defaulting to 15s; attempt number floored at 1; e2e in its own `internal/e2e` package asserting the wire contract).

## Close

After C: fresh Verifier (fresh context, author ≠ verifier) — spec-anchored outcome check over SKEL-01..13 + discrimination sensor over I-WS-1..4; writes `validation.md`; fix loop ≤3.
