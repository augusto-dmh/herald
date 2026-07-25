# RQ05 — Domain model, multi-tenancy, and the drover seam

Research date: 2026-07-25. Scope: herald v0.1's entity set and Postgres schema, tenant isolation and API-key authentication for a machine-first API, payload/response retention, and — centrally — how herald's delivery state machine should sit on top of the drover task queue given drover's actual pre-v0.1 code and roadmap.

## 1. What drover actually provides today (read from source, not guessed)

Everything in section 5 depends on what drover's seam really looks like, so the ground truth first. Cycle A (walking skeleton) is merged; the working tree already contains cycle B's mechanisms (retry policy, `Cancel`/`Snooze`, lease/heartbeat/rescuer, `retryable`/`dead` states) — cycle B is "specified and in flight", so herald must treat it as *about to exist*, not as shipped API.

### 1.1 The enqueue seam

From `/home/augusto/projects/drover/client.go`:

```go
func (c *Client) Insert(ctx context.Context, args JobArgs) (*JobRow, error)
func (c *Client) InsertTx(ctx context.Context, tx pgx.Tx, args JobArgs) (*JobRow, error)
```

`InsertTx` is the flagship: the job row rides the caller's own `pgx.Tx`, so a herald domain write and its job are one WAL commit (ADR-0002: "no ghost jobs, no outbox needed"). Critically, `insertParamsFor` today hardcodes `Queue: defaultQueue` and passes **no** `ScheduledAt`, no per-job `MaxAttempts`, no uniqueness key. There is no `InsertOpts` yet. Per RFC-0001, `ScheduledAt` and named queues arrive in **cycle D**; per-insert `MaxAttempts` appears in no cycle explicitly. This single fact eliminates one whole seam layout for v0.1 (see §5.2).

### 1.2 The execution and retry seam

From `loop.go`, `retry.go`, `errors.go`, `worker.go`:

- The worker loop is **single-flight**: `FetchAvailable(ctx, defaultQueue, lease, 1)` claims one job at a time; per-queue pools with channels are **cycle C**. Until then herald gets exactly one concurrent HTTP delivery per process.
- `RetryPolicy` is a **pluggable client-wide interface** receiving the whole `*JobRow` (`NextRetry(job *JobRow) time.Time`), so it may branch on kind/attempt and return an absolute time. The default is `attempt^4` seconds ±10% jitter (attempt 1 ≈ 1 s, attempt 8 ≈ 68 min, attempt 25 ≈ 4.5 days). **Herald is not stuck with the attempt⁴ shape** — it can install a webhook-shaped schedule (§5.4). This is the load-bearing cycle-B seam.
- `Cancel(reason)` ends a job in `cancelled` (terminal, no more attempts); `Snooze(d)` defers **without consuming an attempt**. Both are recognized through `%w` wrapping (`classifyOutcome` in `errors.go`).
- `max_attempts` defaults to 25 in the schema (`001_create_jobs.sql`) and cannot be set per insert; exhaustion lands the job in retained `dead` state. A worker that wants a smaller cap must enforce it itself via `Cancel` (the `Job[T]` passed to `Work` carries `Attempt`).
- Delivery is at-least-once (ADR-0003): lease + heartbeat + rescuer means a herald delivery handler **may run twice for the same attempt number** after a crash — `FetchExpired` deliberately does not re-increment `attempt`. Herald's attempt-history writes must tolerate this (§5.5).
- The job row accumulates `errors jsonb`, but that column lives and dies with the job row; it is drover's operational breadcrumb, not a queryable delivery log.

### 1.3 What this implies for herald

Herald and drover share one Postgres database and one `pgxpool.Pool`. Herald's tables and `drover_jobs` sit side by side; `drover_jobs` has **no tenant column and cannot get one**, which constrains the multi-tenancy options in §2. Herald's worker code runs inside drover's `Worker[T]` contract: idempotent by contract, per-attempt context, panic-recovered at the job boundary.

## 2. Reference domain models: Svix and Convoy

### 2.1 Svix's entity set

Svix's model (docs.svix.com/overview) is the industry reference for send-side webhook services:

| Entity | Role | Fields that matter |
|---|---|---|
| **Application** | one per consumer/customer of the sender; messages are addressed to it; "completely isolated from one another" | `id`, caller-assigned `uid` (stateless addressing by the sender's own customer ID), `name` |
| **Endpoint** | a delivery target under an application; an application fans out to all its endpoints subject to filters | `id`, `uid`, `url`, signing `secret` (`whsec_` + base64), `description`, `disabled`, `filterTypes` (event-type subscription), `rateLimit` |
| **Event type** | named identifier implying a payload schema; powers endpoint filtering and docs | name, description, (schema) |
| **Message** | the webhook itself; exactly one event type; fans out to matching endpoints | `id`, `eventType`, optional `eventId` (sender's own ID, deduplicating), payload, `payload_retention_period` |
| **Message attempt** | one HTTP delivery try of one message to one endpoint | status (success/pending/fail/sending), `triggerType` (scheduled vs manual), response body, `responseStatusCode`, duration, timestamp, `url` as tried |

Operational behaviors worth copying (docs.svix.com/retries, /retention, /idempotency):

- **Retry schedule**: immediately, 5 s, 5 min, 30 min, 2 h, 5 h, 10 h, 10 h — 8 attempts spread over ~28 h. Only 2xx within 15 s counts as success; 3xx is failure.
- **Exhaustion**: message marked Failed; an operational webhook (`message.attempt.exhausted`) notifies the sender.
- **Automatic endpoint disabling**: when all attempts to an endpoint fail for 5 days, with the clock starting only after failures span ≥12 h inside a 24 h window; a disabled-endpoint notification follows. Manual "Recover Failed" (retry all failed since date) and "Replay Missing" exist.
- **Payload retention**: payloads deleted after 90 days by default, tunable per message; enterprise tier can delete payload on successful delivery. Retention applies to *payloads*, with message metadata outliving them — an argument for separable payload storage (§4).
- **Idempotency**: `Idempotency-Key` request header, same key + same token returns the cached result for up to 12 h.

### 2.2 Convoy's equivalent

Convoy (getconvoy.io/docs) layers **organization → project → {endpoints, events, subscriptions, event deliveries, delivery attempts}**. Projects are typed *incoming* (receive) or *outgoing* (send) — herald v0.1 is outgoing-only. Notable deltas from Svix:

- Convoy has **no application entity**; endpoints carry an `owner_id` string used to group endpoints per consumer and to fan out. Their multi-tenancy guidance is one project per *environment*, with per-customer routing via `owner_id` on the ingested event.
- Project-level config cascades: signature header + hash (default SHA512), retry mechanism (linear/exponential, default 5 attempts), rate limit (default 5 per 5 s), auto-disable of failing endpoints, search-history retention (default 30 days).
- Endpoint states are richer than a boolean: **active → inactive** (consecutive failures; new events become Discarded) → **pending** (retrying its way back) → active, plus **paused** (manual). Endpoints carry per-endpoint rate limits, HTTP timeout, advanced auth (API-key header pair, basic, OAuth2, mTLS in paid tiers).
- Convoy separates **event** (what was ingested) from **event delivery** (the per-endpoint delivery state row) from **delivery attempts** (per-try records) — a three-level split herald should mirror (§5.5, §6).

### 2.3 What herald v0.1 takes from each

Svix's five-entity shape with `uid` addressing and event-type filtering; Convoy's explicit *delivery* row between message and attempts; Svix's retry schedule and disabling trigger as defaults; Convoy's endpoint-state richness deferred (a `disabled` boolean + `disabled_at` + failure-streak columns is enough for v0.1). A full event-type registry table (schemas, docs) is deferred: v0.1 carries `event_type` as a free string on messages plus `filter_types text[]` on endpoints, which buys Svix-style filtered fan-out with zero registry machinery.

## 3. Multi-tenancy in Postgres at herald's scale

### 3.1 The three options honestly

**tenant_id columns + composite indexes** (shared schema, shared tables). Every herald table carries `tenant_id`; every query is written `WHERE tenant_id = $1 AND ...`; every list index leads with the scoping column. Cost: isolation is only as good as query discipline — one forgotten predicate leaks data. Benefit: one migration path, one set of statistics, works with sqlc's static queries, and autovacuum/monitoring see one table per concept.

**Schema-per-tenant**. Strong logical isolation and per-tenant migration/restore, but operationally heavy (N schemas × M tables to migrate, bloated catalogs at high tenant counts) — and for herald it is **structurally incoherent**: `drover_jobs` is a single shared table with no tenant column, so the queue — the busiest table in the system — would remain shared no matter what. Paying schema-per-tenant costs to isolate only the metadata tables buys little.

**Row-Level Security on top of tenant_id**. RLS policies (`USING (tenant_id = current_setting('app.tenant_id')::uuid)`) make the database enforce what the WHERE clause promises — real defense-in-depth against the forgotten predicate. Costs at herald's shape: every request must run inside a transaction that does `SET LOCAL app.tenant_id = ...` (fine with pgx, awkward with transaction-pooling later); the service's own background work (retention pruning, endpoint disabling sweeps, drover's worker loop) needs a bypassing role or `BYPASSRLS`, splitting the connection story; policies show up as init-plans in EXPLAIN and complicate the sqlc-generated query set. RLS protects against herald's *own* bugs, not against other database users — herald v0.1 has exactly one database user.

At v0.1 scale (single Go binary, one privileged role, tens-not-thousands of tenants) the industry consensus (PlanetScale's tenancy survey, RLS practitioner writeups) is that pooled tables with tenant_id are the default, and RLS is the first hardening step *when the query surface stabilizes*. Herald should design every table so RLS can be switched on later without schema change — which costs nothing more than "tenant_id on every tenant-owned row, including denormalized copies on child tables" (§6).

### 3.2 IDs: UUIDv7, and why time-ordering matters

Random UUIDv4 primary keys land at random B-tree leaf positions: page splits, cold cache, WAL amplification on write-heavy tables (messages, delivery_attempts are exactly that). UUIDv7 (RFC 9562) puts a 48-bit Unix-millisecond timestamp in the most significant bits, so new keys append at the right edge of the index — v4-free insert locality with global uniqueness. Postgres 18 ships a native `uuidv7()` with per-backend monotonicity (extra clock-precision bits guarantee each generated value sorts after the previous one within a session); on Postgres < 18 herald can generate v7 in Go (`github.com/google/uuid` supports v7) and the column stays plain `uuid`.

The second benefit is **pagination**: a time-ordered ID makes keyset pagination one-column simple —

```sql
SELECT ... FROM herald_messages
WHERE application_id = $1 AND id < $2   -- $2 = iterator from previous page
ORDER BY id DESC LIMIT 50;
```

— served entirely by an `(application_id, id DESC)` index, no OFFSET scans, and the iterator doubles as a stable cursor because v7 IDs encode creation time. ULID offers the same property with a Crockford-base32 text form; herald prefers UUIDv7 because it stays a native 16-byte `uuid` column (ULID-as-text is 26 bytes plus collation questions) and Postgres is growing native support for it. Public API IDs get Stripe/Svix-style type prefixes at the serialization boundary only (`msg_<uuid>`, `ep_<uuid>`): the prefix makes IDs self-describing in logs and support tickets while the storage stays `uuid`.

## 4. Payload storage, response capture, retention

### 4.1 Where the payload lives

Two layouts:

- **Inline `payload jsonb` on `herald_messages`.** One row per message, one insert, fan-out queries join nothing. Large payloads TOAST out-of-line anyway (>~2 KB post-compression), so the hot part of the row stays small. Retention that deletes whole old messages is a single ranged delete.
- **Separate `herald_message_payloads`.** Needed the moment retention semantics diverge from the envelope's — Svix's model (payload deleted at 90 days *while message metadata persists*, or deleted on successful delivery) is exactly this split. Costs a join on every delivery attempt and a second insert per message.

v0.1 verdict: **inline**, with an enforced ingestion cap (reject bodies > 256 KB at the API; Svix caps payloads far lower in practice) and whole-message retention (§4.3). The separate-table option is the documented upgrade path if payload-only expiry becomes a requirement; the schema change is additive.

### 4.2 Response capture caps

`delivery_attempts` stores what the endpoint answered — status code, duration, and a **truncated** response body, because endpoints can and will return megabytes. Cap at 32 KB of the response body (store `left(body, n)` semantics in the delivery worker, flag truncation), drop response bodies entirely for successful attempts after N days if space matters later. Never store response *headers* wholesale in v0.1 (auth tokens echo back in headers more often than in bodies).

### 4.3 Retention and pruning

Svix's defaults are the reference: 90-day payload retention, per-message override. Herald v0.1: a single retention config per instance (default 90 days), applied by a pruning loop that deletes in bounded batches, child tables first to keep FK checks cheap:

```sql
DELETE FROM herald_delivery_attempts
WHERE id IN (
  SELECT id FROM herald_delivery_attempts
  WHERE attempted_at < now() - $1::interval
  LIMIT 5000);
```

UUIDv7 keys make "older than T" also expressible as `id < uuidv7_boundary(T)` — a pure index range. Time-partitioning `herald_delivery_attempts` by month (prune = `DROP PARTITION`, no dead tuples) is the known scaling move; explicitly deferred past v0.1, and ADR-0002's warning applies to herald's own tables too: MVCC churn from delete-based pruning makes autovacuum settings part of the operational docs. The pruning loop can later become a drover periodic job (drover cycle H, advisory-lock leader election); until then it is a plain ticker goroutine or CLI command — do not block v0.1 on cycle H.

## 5. The drover seam

### 5.1 The design question

A webhook delivery is a small state machine: for each (message, endpoint) pair, try HTTP POST; on failure wait per a schedule and try again; after N failures mark failed and count toward endpoint disabling. Drover is *also* a retry state machine. The question is which machine owns retries, and what a drover "job" denotes.

### 5.2 Layout (a): one drover job per delivery *attempt*

Each attempt is a fire-once job. Herald's worker performs one HTTP call; on failure, herald computes the next attempt time from its own schedule and enqueues a *new* job for it, recording the attempt in its own table; drover-level retries effectively unused (`max_attempts` irrelevant).

- **Fatal for v0.1**: enqueueing "run at now+30 min" requires `ScheduledAt` at insert, which drover only grows in **cycle D**. Without it herald would need its own scheduler loop polling its own table for due attempts — reimplementing drover's core competency (due-time fetch, SKIP LOCKED claim, crash recovery) beside drover. The one honest workaround inside today's API — enqueue immediately and have the worker `Snooze(delay)` until due — converts layout (a) into layout (b) with extra steps.
- What it buys if cycle D existed: total independence from drover's retry semantics; each attempt's enqueue can be transactional with the previous attempt's record (worker opens its own tx: insert attempt row + `InsertTx` next job).
- What it costs regardless: 8× the job rows (one per attempt, ~3 dead tuples each per ADR-0002), and the crash window between "attempt recorded" and "next attempt enqueued" becomes herald's problem — drover's lease/rescuer no longer guarantees the *sequence* continues, only that a single job runs.

### 5.3 Layout (c): herald-owned state machine, drover as dumb executor

Herald tables (`deliveries`) are authoritative for what should happen next; drover jobs are stateless "execute delivery X now" pokes. Pure form has the same problem as (a) — someone must *emit* the poke at the right future time, and that someone is a scheduler herald would have to build. Layout (c)'s real content survives as a **projection**: herald keeps a queryable per-(message, endpoint) delivery row that the worker updates, without herald scheduling anything. That projection is worth keeping whichever layout wins (§5.5).

### 5.4 Layout (b): one drover job per (message, endpoint), drover-native retries — the fit

One job = one delivery lifecycle. Drover owns waiting, claiming, crash recovery, and backoff timing; herald owns the HTTP call, attempt recording, and webhook-domain policy. Three cycle-B seams make herald's policy independent of drover's defaults:

**Retry shape.** `Config.RetryPolicy` is pluggable and receives the whole row, so herald installs the Svix-shaped schedule instead of attempt⁴:

```go
// attempt N failed → wait schedule[N-1] before attempt N+1.
var webhookSchedule = []time.Duration{
    5 * time.Second, 5 * time.Minute, 30 * time.Minute,
    2 * time.Hour, 5 * time.Hour, 10 * time.Hour, 10 * time.Hour,
}

type WebhookRetryPolicy struct{}

func (WebhookRetryPolicy) NextRetry(job *drover.JobRow) time.Time {
    i := min(job.Attempt-1, len(webhookSchedule)-1) // Attempt is 1-based at claim
    return time.Now().Add(webhookSchedule[i])
}
```

The dependency this creates is on the *RetryPolicy interface* (cycle B), not on the attempt⁴ curve. Herald v0.1 therefore hard-requires cycle B merged — a real sequencing constraint, but cycle B is already in drover's working tree.

**Attempt ceiling.** `max_attempts` (25, not settable per insert) is neutralized by herald's worker enforcing its own cap with `Cancel`:

```go
func (w *DeliveryWorker) Work(ctx context.Context, job *drover.Job[DeliverJob]) error {
    d, ep, msg, err := w.load(ctx, job.Args.DeliveryID) // herald rows
    if err != nil { return err }                        // transient: let drover retry
    if ep.Disabled || d.Status == "discarded" {
        w.finalizeDelivery(ctx, d, "discarded")
        return drover.Cancel(errEndpointDisabled)       // terminal, attempts stop
    }
    res := w.deliver(ctx, ep, msg)                      // signed HTTP POST, 15s timeout
    w.recordAttempt(ctx, d, job.Attempt, res)           // §5.5
    if res.Success {
        w.finalizeDelivery(ctx, d, "succeeded")
        return nil
    }
    w.noteEndpointFailure(ctx, ep, res)                 // disabling counters, §5.6
    if job.Attempt >= maxWebhookAttempts {              // 8, Svix-shaped
        w.finalizeDelivery(ctx, d, "failed")
        return drover.Cancel(fmt.Errorf("attempts exhausted: %w", res.Err))
    }
    return res.Err                                      // drover schedules per WebhookRetryPolicy
}
```

**Transactional enqueue — the flagship, and it lands exactly here.** Ingestion accepts a message, fans out to matching endpoints, and enqueues every delivery *in one transaction*; either the message, its delivery rows, and its jobs all exist, or none do:

```go
tx, err := pool.Begin(ctx)
// ... defer rollback
msg := insertMessage(ctx, tx, tenant, app, in)             // herald_messages
eps := matchingEndpoints(ctx, tx, app.ID, in.EventType)    // NOT disabled, filter_types match
for _, ep := range eps {
    d := insertDelivery(ctx, tx, msg.ID, ep.ID)            // herald_deliveries, status=pending
    if _, err := droverClient.InsertTx(ctx, tx, DeliverJob{DeliveryID: d.ID}); err != nil { ... }
}
err = tx.Commit(ctx)                                        // one WAL commit for all of it
```

No outbox, no two-phase anything, no "accepted the message but lost the job" window — this is the mechanism ADR-0002 built drover around, and herald is its intended consumer shape.

### 5.5 Where delivery-attempt history lives

In **herald's `delivery_attempts` table, written by the worker** (`recordAttempt` above), not in drover's `errors jsonb` — drover's column is unqueryable across jobs, capped by row lifetime, and pruned with the job. One subtlety from at-least-once: after a crash between the HTTP call and finalize, the rescuer re-runs the job with the **same** `Attempt` number (drover deliberately doesn't re-increment on rescue). Two attempt rows with the same `(delivery_id, attempt_number)` are then *truthful* — the HTTP call really happened twice. So `delivery_attempts` takes no uniqueness constraint on attempt number; consumers order by `attempted_at`. This is the honest recording of the duplicate window ADR-0003 names.

The **`herald_deliveries` projection** (one row per message×endpoint: status pending → succeeded | failed | discarded, `attempt_count`, `next_attempt_at` optional, `drover_job_id bigint`) exists so dashboards and the API never query `drover_jobs` — drover's table is an internal dependency's internals, its schema can change under herald, and it has no tenant scoping. `drover_job_id` is kept for operator cross-reference only.

### 5.6 Dead state, endpoint disabling, and reconciliation

- **`dead` jobs**: with the worker capping attempts via `Cancel`, a herald job reaches drover-`dead` only pathologically (e.g., 25 straight panics or infra errors before the handler's own logic runs). Herald treats `dead` as an invariant breach to *detect*, not a flow to design around: a low-frequency sweep flags `herald_deliveries` still non-terminal whose job is `dead`/`cancelled`/`completed`, and finalizes them as `failed`. Same sweep covers the crash window between `finalizeDelivery` and drover's own finalize.
- **Endpoint disabling**: computed by herald on failed attempts. v0.1 trigger is deliberately simpler than Svix's 5-day/12-hour rule: disable after K consecutive failed *deliveries* (not attempts) or when `first_failure_at` is older than a configured window with no intervening success — columns `consecutive_failures`, `first_failure_at` on endpoints make both expressible; the thresholds are config. Svix's exact rule is the documented later refinement.
- **Disabling vs in-flight jobs**: disabling **does not** reach into the queue to cancel jobs — there is no drover API for that (job cancellation-by-ID is drover cycle F CLI territory), and it would race claims anyway. Instead every attempt re-checks `endpoint.disabled` at execution time and returns `Cancel` (the worker's first branch above). In-flight and queued deliveries for a disabled endpoint thus drain terminally on their next claim, at the cost of one no-op claim each — bounded and simple. Messages ingested *after* disabling never fan out to that endpoint at all (`NOT disabled` in the fan-out query), matching Convoy's "Discarded" semantics.
- **Manual retry / replay** (post-v0.1 but seam-relevant): a fresh drover job for the same delivery with a `trigger` field in the args — attempt numbering in `delivery_attempts` distinguishes `manual` trigger rows, mirroring Svix's `triggerType`.

### 5.7 Sequencing constraints on drover's roadmap

| Herald need | Drover cycle | Status | Herald v0.1 stance |
|---|---|---|---|
| `InsertTx` transactional enqueue | A | merged | ready |
| RetryPolicy seam, `Cancel`, lease/rescuer, `retryable` | B | in flight | **hard blocker — v0.1 requires cycle B merged** |
| Concurrent deliveries (worker pools, graceful shutdown) | C | future | soft blocker: herald *functions* on the single-flight loop but one slow endpoint (15 s timeout) stalls all delivery; v0.1 launch wants C |
| `ScheduledAt`, named queues, per-insert opts | D | future | not needed by layout (b); would unlock layout (a) and send-at features |
| Metrics (`oldest job age`) | E | future | herald ships its own HTTP metrics regardless |
| Per-insert `max_attempts` | none scheduled | — | worked around via worker-side `Cancel` cap |
| Job cancellation by ID | F (CLI) | future | not needed; disabling handled at execution time |
| Periodic jobs (retention pruning) | H | future | v0.1 uses a plain ticker; migrate later |

The seam choice deliberately minimizes the drover surface herald depends on: cycles A+B only, with C as a throughput (not correctness) dependency.

## 6. v0.1 schema sketch

All IDs `uuid` generated as UUIDv7 in Go (or `DEFAULT uuidv7()` on PG 18). `tenant_id` is denormalized onto every tenant-owned row — including children — so scoped queries never join for authorization and RLS can be enabled later without DDL. Prefixed IDs (`msg_`, `ep_`, `app_`, `key_`) exist only at the API serialization layer.

```sql
CREATE TABLE herald_tenants (
  id          uuid PRIMARY KEY,
  name        text NOT NULL,
  created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE herald_api_keys (
  id           uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL REFERENCES herald_tenants(id),
  token_hash   bytea NOT NULL,            -- SHA-256 of the full secret; the secret itself is never stored
  token_prefix text NOT NULL,             -- e.g. 'hrld_live_3fk2' — display + support lookup only
  name         text NOT NULL DEFAULT '',
  scope        text NOT NULL DEFAULT 'full'
               CHECK (scope IN ('full','ingest')),   -- §7.3
  last_used_at timestamptz,               -- throttled updates, §7.2
  created_at   timestamptz NOT NULL DEFAULT now(),
  revoked_at   timestamptz                -- soft revoke: key rows are audit history
);
CREATE UNIQUE INDEX herald_api_keys_hash_idx ON herald_api_keys (token_hash);
CREATE INDEX herald_api_keys_tenant_idx ON herald_api_keys (tenant_id, id);

CREATE TABLE herald_applications (
  id          uuid PRIMARY KEY,
  tenant_id   uuid NOT NULL REFERENCES herald_tenants(id),
  uid         text,                       -- caller-assigned stable id (Svix uid): address consumers
  name        text NOT NULL,              --   by the sender's own customer key, statelessly
  created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX herald_applications_uid_idx
  ON herald_applications (tenant_id, uid) WHERE uid IS NOT NULL;
CREATE INDEX herald_applications_list_idx ON herald_applications (tenant_id, id DESC); -- keyset pages

CREATE TABLE herald_endpoints (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL REFERENCES herald_tenants(id),
  application_id       uuid NOT NULL REFERENCES herald_applications(id),
  url                  text NOT NULL,       -- https enforced at API layer (+ SSRF checks, other RQ)
  secret               text NOT NULL,       -- 'whsec_'+base64(32B); retrievable for signing → not hashed
  description          text NOT NULL DEFAULT '',
  filter_types         text[],              -- NULL = all event types (Svix filterTypes)
  disabled             boolean NOT NULL DEFAULT false,
  disabled_at          timestamptz,
  consecutive_failures int  NOT NULL DEFAULT 0,   -- §5.6 disabling counters
  first_failure_at     timestamptz,
  created_at           timestamptz NOT NULL DEFAULT now(),
  updated_at           timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX herald_endpoints_list_idx ON herald_endpoints (application_id, id DESC);
CREATE INDEX herald_endpoints_fanout_idx ON herald_endpoints (application_id)
  WHERE NOT disabled;                       -- the ingestion fan-out query, hot path

CREATE TABLE herald_messages (
  id             uuid PRIMARY KEY,          -- UUIDv7: doubles as creation-time cursor
  tenant_id      uuid NOT NULL REFERENCES herald_tenants(id),
  application_id uuid NOT NULL REFERENCES herald_applications(id),
  event_type     text NOT NULL,
  event_id       text,                      -- sender's own id; dedupes redelivered POSTs
  payload        jsonb NOT NULL,            -- inline, ≤256 KB enforced at API (§4.1)
  created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX herald_messages_event_id_idx
  ON herald_messages (application_id, event_id) WHERE event_id IS NOT NULL;
CREATE INDEX herald_messages_list_idx ON herald_messages (application_id, id DESC);
CREATE INDEX herald_messages_tenant_list_idx ON herald_messages (tenant_id, id DESC);

CREATE TABLE herald_deliveries (             -- Convoy's "event delivery": queryable projection §5.5
  id            uuid PRIMARY KEY,
  tenant_id     uuid NOT NULL,
  message_id    uuid NOT NULL REFERENCES herald_messages(id) ON DELETE CASCADE,
  endpoint_id   uuid NOT NULL REFERENCES herald_endpoints(id),
  status        text NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','succeeded','failed','discarded')),
  attempt_count int NOT NULL DEFAULT 0,
  drover_job_id bigint,                      -- operator cross-reference only; never joined in API paths
  created_at    timestamptz NOT NULL DEFAULT now(),
  finalized_at  timestamptz
);
CREATE UNIQUE INDEX herald_deliveries_pair_idx ON herald_deliveries (message_id, endpoint_id);
CREATE INDEX herald_deliveries_endpoint_idx ON herald_deliveries (endpoint_id, id DESC);
CREATE INDEX herald_deliveries_reconcile_idx ON herald_deliveries (status, created_at)
  WHERE status = 'pending';                  -- §5.6 reconciliation sweep

CREATE TABLE herald_delivery_attempts (
  id                   uuid PRIMARY KEY,
  tenant_id            uuid NOT NULL,
  delivery_id          uuid NOT NULL REFERENCES herald_deliveries(id) ON DELETE CASCADE,
  endpoint_id          uuid NOT NULL,        -- denormalized: endpoint-health queries skip a join
  attempt_number       int NOT NULL,         -- duplicates possible after rescue; no unique (§5.5)
  trigger_type         text NOT NULL DEFAULT 'automatic'
                       CHECK (trigger_type IN ('automatic','manual')),
  succeeded            boolean NOT NULL,
  response_status_code int,                  -- NULL when the request never completed
  response_body        text,                 -- first 32 KB (§4.2)
  response_truncated   boolean NOT NULL DEFAULT false,
  duration_ms          int NOT NULL,
  error                text,                 -- transport error when no HTTP response exists
  attempted_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX herald_attempts_delivery_idx ON herald_delivery_attempts (delivery_id, attempted_at);
CREATE INDEX herald_attempts_endpoint_idx ON herald_delivery_attempts (endpoint_id, attempted_at DESC);
CREATE INDEX herald_attempts_failures_idx ON herald_delivery_attempts (endpoint_id, attempted_at DESC)
  WHERE NOT succeeded;                       -- disabling logic + "recent failures" dashboard
```

Index rationale: every list endpoint pages by `(scope_column, id DESC)` keyset (§3.2); the fan-out partial index serves the single hottest query; the failures partial index keeps the disabling computation off the full attempts heap; `tenant_id` appears on children so `GET /attempts?tenant=X`-class queries and future RLS need no joins. Pruning (§4.3) deletes attempts, then deliveries, then messages, oldest-first by UUIDv7 range.

## 7. API authentication for a machine-first API

### 7.1 Why API keys, not OAuth or JWT

Herald's callers are servers, not users: no delegation, no consent, no third-party audience — the problems OAuth exists to solve are absent, and standing up token endpoints + client-credential flows is pure surface. Stateless JWTs (what self-hosted Svix uses: bearer JWTs signed with an instance secret) trade the per-request DB lookup for an unsolvable revocation story — revoking one leaked token requires a denylist, i.e., the database lookup you removed — and a shared signing secret makes per-tenant issuance and audit awkward. A DB-backed key is one indexed point lookup per request against a table herald already owns, gives instant revocation (`revoked_at`), per-key audit (`last_used_at`), and per-key scope. At v0.1 scale the lookup cost is noise.

### 7.2 Key format and handling (Stripe-shaped)

- **Format**: `hrld_<mode>_<43 chars base62 of 32 random bytes>` — e.g. `hrld_live_x7Kp...`. The prefix makes keys self-identifying in logs and enables secret-scanning rules (the reason `sk_live_` exists); a `test` mode is cheap to reserve now even if unused in v0.1.
- **Hashing at rest**: store only `sha256(token)`; lookup is `WHERE token_hash = $1` on the unique index. SHA-256 (not bcrypt) is correct for 256-bit random secrets — there is nothing to brute-force, and bcrypt would add ~100 ms per request. Hash-then-index also makes timing attacks on comparison moot; compare with `subtle.ConstantTimeCompare` anyway on the retrieved row.
- **Shown once**: the plaintext appears only in the create response (Stripe's model); `token_prefix` (first ~14 chars) is stored for display and support.
- **`last_used_at`**: updated at most once per minute per key (in-memory throttle), not per request — an unthrottled hot-row UPDATE on every API call is a classic self-inflicted contention + bloat source. Powers "is this key still in use?" before revocation, per Stripe's rotation guidance.
- **Rotation**: create-new + revoke-old with both valid during overlap; no forced grace-period machinery in v0.1.

### 7.3 Management vs ingestion separation

One HTTP server, one auth middleware, **scoped keys** rather than separate APIs: `scope='ingest'` may only `POST /v1/apps/{app}/messages`; `scope='full'` may also manage applications/endpoints/keys and read delivery logs. This captures the real security asymmetry — ingestion keys live in many producing services and leak more often; a leaked ingest key can inject events but cannot read logs, rewrite endpoint URLs (exfiltration vector), or mint keys. Separate ports/services for the two planes is an operational split herald does not need at v0.1. Endpoint *signing* secrets (`whsec_`) are unrelated to API auth and stay retrievable (§6).

## Comparison table

Seam layouts from §5, against what matters:

| Dimension | (a) job per attempt | (b) job per delivery, drover retries ★ | (c) herald state machine, drover as executor |
|---|---|---|---|
| Retry timing owned by | herald (needs own scheduler or cycle-D `ScheduledAt`) | drover, shaped by herald's `RetryPolicy` | herald (needs own scheduler) |
| Works on drover cycles A+B only | **No** (blocked on D) | **Yes** | No (same scheduling gap as (a)) |
| Retry schedule = Svix shape | yes (herald computes) | yes (custom `RetryPolicy`, §5.4) | yes (herald computes) |
| Crash recovery of the *sequence* | herald's problem between attempts | drover's lease/rescuer end-to-end | herald's problem |
| Transactional ingest fan-out (`InsertTx`) | yes | yes | yes |
| Attempt history | herald table (natural) | herald table, written by worker; rescue duplicates possible (§5.5) | herald table (natural) |
| Attempt cap ≠ 25 | trivial (unused) | worker-side `Cancel` at cap | trivial |
| Endpoint disabling of queued work | herald skips enqueue | re-check + `Cancel` at claim time | herald marks own rows |
| Job rows / MVCC churn | ~8× per failed delivery | 1 per delivery | 1 per attempt or per poke |
| Herald code to write | scheduler + retry logic | HTTP + recording + policy plugs | scheduler + full state machine |
| Coupling to drover internals | low | medium (RetryPolicy, Cancel semantics, attempt numbering) | lowest |
| Queryability without touching `drover_jobs` | good | good **with `herald_deliveries` projection** | best |

## Gap analysis

**Drover gaps herald must absorb (v0.1):** no per-insert `MaxAttempts`/queue/`ScheduledAt` (worked around via worker-side `Cancel` cap; named queues and send-at features wait for cycle D); single-flight worker loop until cycle C — one slow endpoint head-of-line-blocks all delivery, making cycle C the throughput gate for any real launch; no completion/failure hooks or middleware until cycle D, so all herald bookkeeping happens inside the worker body; `RetryPolicy` is client-wide (acceptable: herald v0.1 has one job kind, and the interface receives the row for future branching); no job-cancellation API, so endpoint disabling must drain via claim-time checks; drover's `errors jsonb` grows per attempt but herald's 8-attempt cap bounds it.

**Herald-side gaps this research leaves open (other RQs or later):** signing scheme and header format (secret storage assumed retrievable here); SSRF/egress protection on endpoint URLs; rate limiting per endpoint (column reserved, mechanism unspecified); Svix's exact 5-day disabling rule vs the simpler v0.1 trigger; operational webhooks (`message.attempt.exhausted`-style meta-events); manual retry/recover/replay APIs; payload-only retention (separate payload table); partitioning `delivery_attempts`; RLS enablement plan; multi-instance herald (two processes running drover loops is safe by drover's design — SKIP LOCKED — but herald's disabling counters then race benignly; worth one test).

**Verification gaps:** Svix `MessageAttemptOut` exact field enums and any response-body truncation cap were not retrievable from the API reference page (JS-rendered); the 32 KB cap here is herald's own choice, not a copied number.

## Options

**Option 1 — Entity set: tenant → application → endpoint (Svix-shaped, with `herald_deliveries` projection) — ★ RECOMMENDED.** Why: the application layer is what makes the ingest API make sense — senders address *their customer* (`app uid`), herald fans out to that customer's endpoints; without it, senders must track herald endpoint IDs per consumer and fan-out semantics collapse into point-to-point sends. It is one small table, and both references converge on the concept (Svix `application`, Convoy `owner_id`). The deliveries projection keeps drover's tables out of the API surface. Why not the flat alternative (tenant → endpoint, Convoy-style `owner_id` string on endpoints): saves one table but loses uid-anchored uniqueness, per-consumer listing, and a natural home for future per-consumer settings; the savings are not worth the API-shape damage. Event-type *registry* stays out of v0.1 (free-string `event_type` + `filter_types[]`).

**Option 2 — Drover seam: layout (b), one job per (message, endpoint), drover-native retries with herald's `RetryPolicy` and worker-side `Cancel` cap, plus the `herald_deliveries` projection — ★ RECOMMENDED.** Why: it is the only layout that works on drover cycles A+B alone; it hands the genuinely hard machinery (due-time claiming, lease/heartbeat/rescuer crash recovery, backoff persistence) to the component built for it while the pluggable `RetryPolicy` keeps herald's Svix-shaped schedule fully herald-owned; and it lands `InsertTx` exactly where transactional enqueue pays — atomic message+deliveries+jobs ingestion. Why not (a)/(c): both require a herald-built scheduler (or drover cycle D) to place future attempts, duplicating drover's core inside its consumer; (a) additionally multiplies job rows ~8× per failed delivery. Accepted costs: dependence on cycle-B semantics (attempt numbering, `Cancel`, policy interface), rescue-duplicate attempt rows recorded truthfully, and a small reconciliation sweep for the dead/cancelled edge.

**Option 3 — API auth: prefixed, SHA-256-hashed, tenant-scoped API keys (`hrld_live_…`) with `full`/`ingest` scopes on a single API — ★ RECOMMENDED.** Why: machine-first callers need revocable, auditable, per-tenant credentials with zero ceremony; hash-at-rest with shown-once plaintext and throttled `last_used_at` is the Stripe-proven shape; scope separation captures the leak-risk asymmetry between producing services and admin tooling without running two APIs. Why not OAuth2 client-credentials or Svix-style instance-secret JWTs: OAuth adds an authorization server for a delegation problem herald doesn't have; stateless JWTs forfeit instant revocation and per-key audit, and regain them only by re-adding the database lookup keys already do.

## Sources

- /home/augusto/projects/drover/README.md — positioning, planned v0.1 API, roadmap summary (accessed 2026-07-25)
- /home/augusto/projects/drover/docs/adr/0001..0004-*.md — scope, Postgres-only + InsertTx flagship, at-least-once lease/heartbeat/rescuer + retry semantics, layout/toolchain (accessed 2026-07-25)
- /home/augusto/projects/drover/docs/rfc/0001-drover-roadmap.md — cycle contents A–I, cut line, out-of-scope walls (accessed 2026-07-25)
- /home/augusto/projects/drover/client.go, job.go, worker.go, loop.go, retry.go, errors.go — actual `Insert`/`InsertTx` signatures (no insert options), single-flight fetch loop, pluggable `RetryPolicy`, `Cancel`/`Snooze` classification (accessed 2026-07-25)
- /home/augusto/projects/drover/internal/driver (driver.go) and internal/migrate/migrations/001,002 — jobs schema, states, fetch/lease indexes, rescue-does-not-reincrement-attempt contract (accessed 2026-07-25)
- https://docs.svix.com/overview (accessed 2026-07-25) — application/endpoint/event-type/message/attempt entity definitions, uid addressing
- https://docs.svix.com/retries (accessed 2026-07-25) — retry schedule, 15 s/2xx success rule, exhaustion webhook, 5-day auto-disable rule, recover/replay
- https://docs.svix.com/retention (accessed 2026-07-25, via search summary) — 90-day payload retention, per-message `payload_retention_period`, delete-on-delivery tier
- https://docs.svix.com/idempotency (accessed 2026-07-25, via search summary) — `Idempotency-Key` header, ~12 h replay window
- https://api.svix.com/docs (accessed 2026-07-25) — attempted for `MessageAttemptOut` field detail; page is JS-rendered, fields not extractable (noted in gap analysis)
- https://getconvoy.io/docs/product-manual/organizations-and-projects (accessed 2026-07-25) — org/project hierarchy, incoming vs outgoing, project-level retry/signature/rate-limit defaults, owner_id tenancy guidance
- https://getconvoy.io/docs/product-manual/endpoints (accessed 2026-07-25) — endpoint fields, secret rotation, per-endpoint rate limit/timeout/auth, active/inactive/pending/paused states
- https://docs.stripe.com/keys (accessed 2026-07-25) — key prefixes, restricted keys, shown-once secrets, rotation and last-used-before-revoke guidance
- https://planetscale.com/blog/approaches-to-tenancy-in-postgres (accessed 2026-07-25, via search summary) — tenant_id vs schema vs RLS trade-off survey
- https://www.thenile.dev/blog/uuidv7 and https://neon.com/postgresql/18/uuidv7-support (accessed 2026-07-25, via search summaries) — PG 18 native `uuidv7()`, B-tree locality, per-backend monotonicity

**Unverified claims flagged:** Svix's `MessageAttemptOut` field list and status/trigger enums in §2.1's attempt row are reconstructed from the overview page plus prior knowledge — the API reference itself would not render for extraction, and any response-body truncation limit Svix applies is unknown (herald's 32 KB cap is an independent choice). The Svix retention and idempotency details (90 days, 12 h window) came from search-result summaries of the official docs pages rather than direct page fetches. Convoy's default retry count (5) and rate limit (5 per 5 s) reflect the fetched docs at access time and may drift. The claim that drover cycle B will merge with the `RetryPolicy`/`Cancel` semantics exactly as present in today's working tree is a projection from in-flight code, not a released API contract. `github.com/google/uuid` UUIDv7 generation support is from prior knowledge and was not re-verified against its current release. All drover behavior claims were verified directly against source at the paths listed above.
