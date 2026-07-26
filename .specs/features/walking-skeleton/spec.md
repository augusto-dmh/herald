# Spec — `walking-skeleton`

## Problem Statement

Herald has decisions (ADR-0001..0006) and a roadmap but no running system. This cycle builds the thinnest end-to-end slice: a message POSTed to an authenticated HTTP API is persisted, fanned out transactionally into drover jobs, delivered (unsigned) to registered endpoints by a worker, and its attempt is queryable back through the API.

## Goals

- Full v0.1 schema in place so later cycles alter, not rebuild.
- Transactional ingest: message, its deliveries, and their drover jobs commit atomically.
- A drover-executed delivery worker that POSTs and records what happened.
- Tenant-scoped, API-key-authenticated HTTP surface.

## Out of Scope

| Item | Reason |
|---|---|
| Signing, secrets rotation | Cycle B (schema table ships now, unused) |
| Retry ladder, endpoint disabling, redelivery | Cycle C (failed jobs go `dead` under drover cycle-A semantics; delivery marked `failed` on first failure) |
| SSRF dialer, timeout taxonomy, politeness | Cycle D (a plain 15s client timeout only) |
| Pagination, filters, idempotency keys | Cycle E (single-resource GETs only) |
| Metrics, health endpoints, serve binary, compose | Cycle F (e2e test wires server + worker in-process) |

## Assumptions & Open Questions

| Assumption | Chosen default | Rationale | Confirmed? |
|---|---|---|---|
| Query layer | Hand-written pgx queries in a store package | Small auditable core; sqlc + drift CI deferred until query count grows | y (auto, D-1) |
| Herald migrations | Own embed.FS runner + `herald_migrations` version table, drover's schema applied via `drover.Migrate` | Mirrors the proven drover pattern; no third-party migrator | y (auto, D-2) |
| Tenant creation | `POST /v1/tenants` guarded by `HERALD_BOOTSTRAP_TOKEN` env; returns tenant + first API key once | Tenant self-signup is a product decision beyond this cycle; operator bootstrap is minimal and e2e-testable | y (auto, D-3) |
| HTTP layer | stdlib `net/http` with Go 1.22 method+pattern routing | No framework dependency; auditable core | y (auto, D-4) |
| Job granularity | One drover job per delivery, args = delivery ID only | ADR-0005; worker re-reads state so stale payloads are impossible | y (auto, D-5) |
| Drover dependency | `github.com/augusto-dmh/drover@main` pseudo-version | Cycle A is merged upstream; no tag exists yet | y (auto, D-6) |
| IDs | UUIDv7 via `github.com/google/uuid` (`NewV7`) | ADR-0002 | y (auto, D-7) |

## User Stories

### P1: Operator bootstraps a tenant ⭐ MVP

**User Story**: As an operator, I POST to `/v1/tenants` with the bootstrap token to create a tenant and receive its first API key, shown exactly once.

**Acceptance Criteria**:
1. WHEN `POST /v1/tenants` carries the correct bootstrap token THEN the system SHALL create the tenant and return 201 with the tenant and a `hrld_live_`-prefixed API key in plaintext.
2. WHEN the bootstrap token is missing or wrong THEN the system SHALL return 401 and create nothing.
3. WHEN an API key is stored THEN the system SHALL persist only its SHA-256 hash and display prefix, never the plaintext.

**Independent Test**: create a tenant via the API, verify 201 and key shape, verify the DB row holds a hash that matches the returned key and no plaintext.

### P2: Tenant manages applications and endpoints ⭐ MVP

**User Story**: As a tenant, I create an application and register endpoint URLs under it using my API key.

**Acceptance Criteria**:
1. WHEN `POST /v1/applications` carries a valid `full`-scope key THEN the system SHALL create the application scoped to that key's tenant and return 201.
2. WHEN `POST /v1/applications/{uid}/endpoints` names an application of the caller's tenant THEN the system SHALL create the endpoint (URL, optional `filter_types`) and return 201.
3. WHEN any management call carries no key, an unknown key, or an `ingest`-scope key THEN the system SHALL return 401 (missing/unknown) or 403 (insufficient scope).
4. WHEN a call names an application belonging to a different tenant THEN the system SHALL return 404.

**Independent Test**: two tenants; tenant B addressing tenant A's application uid gets 404; ingest-scoped key on management gets 403.

### P3: Message ingest fans out transactionally ⭐ MVP

**User Story**: As a tenant's application, I POST a message once and herald creates one delivery per matching endpoint, each backed by a drover job, atomically.

**Acceptance Criteria**:
1. WHEN `POST /v1/applications/{uid}/messages` carries a valid key (`full` or `ingest`) and a body with `event_type` and JSON `payload` THEN the system SHALL insert the message, one `deliveries` row per matching enabled endpoint, and one drover job per delivery, in a single transaction, and return 202 with the message ID.
2. WHEN an endpoint's `filter_types` is set THEN it SHALL match only messages whose `event_type` is in the list; a NULL `filter_types` SHALL match all.
3. WHEN the transaction fails at any point THEN the system SHALL leave no message, delivery, or job rows behind.
4. WHEN the request body exceeds 1 MiB or is not valid JSON THEN the system SHALL return 413 or 422 respectively and enqueue nothing.
5. WHEN no endpoint matches THEN the system SHALL store the message with zero deliveries and still return 202.

**Independent Test**: app with three endpoints (one filtered out, one disabled), one message → exactly one delivery+job pair for the matching enabled endpoint, all rows sharing one transaction's visibility.

### P4: Deliveries execute and attempts are queryable ⭐ MVP

**User Story**: As a tenant, after ingesting a message I can see each delivery's attempt — status code, success, timing — via the API.

**Acceptance Criteria**:
1. WHEN a delivery job runs THEN the worker SHALL POST the message payload as `application/json` to the endpoint URL with a 15s timeout and record exactly one `delivery_attempts` row (status code, success, error text, capped response snippet, duration).
2. WHEN the response is 2xx THEN the delivery SHALL be marked `delivered`; on any other outcome (non-2xx, timeout, connection error) it SHALL be marked `failed` and the job SHALL not report success to drover.
3. WHEN `GET /v1/applications/{uid}/messages/{id}` is called by the owning tenant THEN the system SHALL return the message with its deliveries and their attempts.
4. WHEN the message belongs to another tenant THEN the system SHALL return 404.

**Independent Test**: e2e — bootstrap tenant → create app + endpoint (local receiver) → POST message → drover loop delivers → GET shows `delivered` with one successful attempt carrying the receiver-observed status code.

## Edge Cases

- Ingest for an application uid that does not exist under the caller's tenant → 404, nothing stored.
- Receiver returns 500 → attempt recorded `success=false` with status 500, delivery `failed`.
- Receiver unreachable (connection refused) → attempt recorded with empty status code, error text set, delivery `failed`.
- Response body larger than the snippet cap → snippet truncated, delivery outcome unaffected.
- Two endpoints match one message → two independent deliveries; one failing does not affect the other.

## Invariants

- I-WS-1: a committed message has exactly one drover job per its deliveries — never a message whose jobs are missing (transactional ingest).
- I-WS-2: no API response ever contains rows belonging to another tenant.
- I-WS-3: every executed delivery job leaves exactly one attempt row per execution.
- I-WS-4: API-key plaintext exists only in the 201 response of its creation; the database holds only hash + display prefix.

## Requirement Traceability

| ID | Requirement | Story |
|---|---|---|
| SKEL-01 | Bootstrap-token tenant creation returning one-time key | P1 |
| SKEL-02 | Hashed-at-rest API keys with display prefix | P1 |
| SKEL-03 | Bearer auth middleware resolving tenant + scope | P2 |
| SKEL-04 | Application create under authenticated tenant | P2 |
| SKEL-05 | Endpoint create with URL + optional filter_types | P2 |
| SKEL-06 | Cross-tenant addressing yields 404; wrong scope 403 | P2, P4 |
| SKEL-07 | Transactional ingest: message + deliveries + jobs atomic | P3 |
| SKEL-08 | filter_types matching incl. NULL-matches-all and disabled exclusion | P3 |
| SKEL-09 | Body limits: 1 MiB cap, JSON validation | P3 |
| SKEL-10 | Delivery worker POSTs payload, records one attempt per execution | P4 |
| SKEL-11 | Delivery status transitions pending→delivered/failed on 2xx rule | P4 |
| SKEL-12 | Message detail endpoint returning deliveries + attempts | P4 |
| SKEL-13 | Full v0.1 schema migrations incl. tables unused this cycle | — |

## Success Criteria

The RFC-0001 cycle-A exit: an e2e test proves a message POSTed to the API reaches a local receiver and its attempt is queryable through the API — plus all gates green in CI.

## Dimensions Sweep

- **Input validation**: URL/event_type/payload validated at handlers (SKEL-09); endpoint URL must parse as http(s).
- **Failure / partial failure**: SKEL-07 atomicity; worker failure path AC P4.2; per-delivery independence (edge cases).
- **Idempotency**: N/A this cycle — idempotency keys are cycle E (RFC); duplicate POSTs create duplicate messages by design for now.
- **Auth**: SKEL-01..03, SKEL-06.
- **Concurrency / ordering**: single drover loop (upstream cycle-A semantics); no ordering guarantees per ADR-0003. Attempt rows keyed by delivery + execution, not unique on attempt number (ADR-0005).
