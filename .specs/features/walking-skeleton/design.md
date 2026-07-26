# Design — `walking-skeleton`

## Approach

Layered architecture, idiomatic Go — deliberately not hexagonal: herald has one database, one queue, and one outbound protocol, so ports-and-adapters would abstract axes that cannot vary, against ADR-0001's "read the whole delivery path in one sitting". Boundaries are Go package boundaries under `internal/`; the one seam that earns indirection is the store (testability against real Postgres), and even it is a concrete type, not an interface.

## Architecture Overview

```
                    ┌────────────────────────────────────────────┐
                    │                 Postgres                    │
                    │  herald tables ── drover tables (shared DB) │
                    └───────▲───────────────────▲────────────────┘
                            │ pgx               │
   HTTP client ──▶ internal/httpapi ──▶ internal/store ──▶ pgxpool
                    auth middleware      hand-written queries
                            │ InsertTx (same tx as message rows)
                            ▼
                    drover.Client ──▶ internal/deliver.Worker ──▶ endpoint URL
                    (poll loop)        POST + record attempt
```

## Components

- **`internal/herald`** — domain types (Tenant, APIKey, Application, Endpoint, Message, Delivery, DeliveryAttempt) + enums (Scope, DeliveryStatus). No behavior beyond validation helpers. UUIDv7 IDs.
- **`internal/store`** — `Store{pool}` with per-entity methods; multi-statement writes take `pgx.Tx`. Ingest exposes `CreateMessageTx(tx, …)` etc. so the handler composes one transaction: message + deliveries + `droverClient.InsertTx` per delivery.
- **`internal/migrate`** — embed.FS SQL migrations + `herald_migrations` version table (drover's runner pattern); `Migrate(ctx, pool)` applies herald's schema, caller also runs `drover.Migrate`.
- **`internal/httpapi`** — `NewServer(store, drover, cfg)` returning `http.Handler`; Go 1.22 pattern routing; middleware chain: body cap → auth (Bearer `hrld_live_…` → SHA-256 → key row → tenant+scope in context) except `POST /v1/tenants` which checks the bootstrap token.
- **`internal/deliver`** — `Args{DeliveryID}` (`Kind() = "webhook_delivery"`), `Worker` with `http.Client{Timeout: 15s}`; loads delivery+endpoint+message, POSTs, writes the attempt row + delivery status, returns error on non-2xx so drover records the job failed.
- **`internal/testdb`** — testcontainers Postgres helper shared by integration tests (mirrors drover's).

## Routes (v0.1 slice)

| Method/Path | Auth | Action |
|---|---|---|
| POST `/v1/tenants` | bootstrap token | create tenant + first key (once-only plaintext) |
| POST `/v1/applications` | key, `full` | create application |
| POST `/v1/applications/{uid}/endpoints` | key, `full` | create endpoint |
| POST `/v1/applications/{uid}/messages` | key, any scope | transactional ingest, 202 |
| GET `/v1/applications/{uid}/messages/{id}` | key, `full` | message + deliveries + attempts |

## Data Model

Migration 0001 creates the full v0.1 set (ADR-0002): `tenants`, `api_keys` (key_hash bytea unique, prefix, scope check full|ingest, last_used_at), `applications` (unique tenant_id+uid), `endpoints` (url, filter_types text[] NULL=all, disabled bool), `endpoint_secrets` (unused until cycle B), `messages` (event_type, payload jsonb), `deliveries` (unique message_id+endpoint_id, status pending|delivered|failed), `delivery_attempts` (attempt rows, no unique on attempt number per ADR-0005). Composite indexes lead with `tenant_id`; children carry `tenant_id` denormalized for isolation-by-construction queries.

## Error Handling Strategy

| Case | Behavior |
|---|---|
| Unknown/missing key | 401 JSON error |
| Insufficient scope | 403 |
| Cross-tenant or missing resource | 404 (never 403 — no existence leak) |
| Body > 1 MiB | 413 via `http.MaxBytesReader` |
| Invalid JSON / missing fields | 422 with field message |
| Store/DB error | 500, logged via slog, no internals in body |
| Delivery non-2xx/transport error | attempt recorded, delivery `failed`, job error returned to drover (→ `dead` under cycle-A semantics; retries are cycle C upstream+here) |

## Tech Decisions

| Decision | Choice | Note |
|---|---|---|
| Query layer | pgx v5 raw | D-1; revisit at ~30+ queries |
| Router | stdlib ServeMux patterns | D-4 |
| IDs | `uuid.NewV7()` | D-7 |
| Drover pin | `@main` pseudo-version | D-6; upgrade to tag at drover v0.1.0 |
| Key hashing | SHA-256 (not bcrypt) | high-entropy random keys need no KDF; indexable equality lookup |

## Risks & Concerns

| Risk | Mitigation |
|---|---|
| Drover `@main` moves under us | pseudo-version pins a commit; upgrades are explicit |
| Failed deliveries dead-end (no retries yet) | scoped: RFC cycle C; e2e asserts the recorded truth, not recovery |
| Shared DB schema collision with drover tables | drover tables are `drover_`-prefixed upstream; herald tables unprefixed |
