# RQ03 — Competitive landscape and product scope

Research date: 2026-07-25. Scope: survey of Svix, Convoy, and Hookdeck Outpost (plus the Stripe/GitHub build-it-yourself baseline and Standard Webhooks) to derive the table-stakes feature set, self-hosting complexity comparison, and a recommended v0.1 scope cut for herald.

## 1. Svix — the category-defining hosted service with an OSS server

### 1.1 Domain model

Svix's domain model is the de facto vocabulary of the category and worth adopting almost verbatim:

- **Application** — the tenant-like unit; one per *consumer* of webhooks (i.e., one per customer of the sender). All messages and endpoints hang off an application.
- **Endpoint** — a URL registered under an application, with its own signing secret, optional event-type filter, and enable/disable state.
- **Message** — one event published into an application; fan-out to all matching endpoints is implicit.
- **Message attempt** — one concrete HTTP delivery try, recording status code, response body excerpt, and timing.
- **Event type** — a named, schema-optional category (`invoice.paid`) that endpoints can subscribe to selectively.

### 1.2 Delivery mechanics (the reference behavior)

- Retry schedule: exponential-ish fixed ladder — immediately, 5s, 5m, 30m, 2h, 5h, 10h, 10h. Success is a 2xx within 15 seconds; 3xx redirects count as failures.
- **Automatic endpoint disabling**: an endpoint is disabled when all attempts fail for 5 days, with guards (multiple failures within 24h, ≥12h between first and last) so a brief outage doesn't disable anyone. Can be turned off.
- Manual recovery: per-message retry, "Recover Failed" (bulk retry failed since date), and "Replay Missing" (send messages never attempted, e.g. created while endpoint was disabled).

### 1.3 OSS server vs hosted

The OSS server (`svix/svix-webhooks`, MIT) is written in **Rust**, requires **PostgreSQL plus Redis** (Redis 6.2+, persistence on, `noeviction` policy — i.e., Redis used as a durable queue, not just a cache). It covers signing (HMAC-SHA256 family plus Ed25519), retries, SSRF protection via subnet allowlists, and operational webhooks. The hosted product layers on the commercial surface: consumer portal customization, transformations, connectors, object-storage/broker/database endpoints, static IPs, FIFO ordering, polling endpoints, custom retry schedules, OTel streaming, SSO/audit logs (Enterprise). Pricing tiers run Free → Professional ($490/mo+) → Enterprise; notably the pricing page doesn't even mention the OSS server — Svix positions self-hosting as an afterthought, not a product line.

### 1.4 Takeaway for herald

Svix defines both the vocabulary and the expected delivery semantics (retry ladder, auto-disable with hysteresis, replay). Its OSS server is credible but Rust + Postgres + carefully-configured-Redis is a nontrivial self-host, and the OSS build is explicitly the un-optimized sibling of the hosted product.

## 2. Convoy — Go webhooks gateway, feature-rich and operationally heavy

### 2.1 Architecture and infrastructure

Convoy (frain-dev/convoy, Go with an Angular dashboard) runs as **three service roles**:

1. **Server** — stateless REST API powering dashboard and SDKs; events go to Redis immediately, then persist to Postgres.
2. **Agent** — async task processing (delivery, maintenance) and message-broker ingestion; stateless, horizontally scaled.
3. **Egress** — an HTTP CONNECT proxy for static IPs and SSRF protection; docs recommend ≥3 instances.

Required infrastructure: **PostgreSQL 15+** (primary store and search) and **Redis 6+** (cache, job queue, rate limiting, circuit breaking), with PgBouncer and Redis Sentinel pages in the deployment docs — a signal of the operational posture expected. Production guidance is load-balanced servers, multiple agents, ≥3 egress instances, Postgres with replicas, Redis with Sentinel.

Convoy is instructive history: it originally ran on **MongoDB** and migrated to Postgres in v0.9, citing self-hosted users needing replica sets just to get transactions, the lack of schema-migration tooling, and slow queries — the API got noticeably faster after the move. Even the incumbents converged on Postgres as the right store for this workload.

### 2.2 Features and license

Feature set is broad: payload signing, constant-time and exponential backoff retries, batch retries, **per-endpoint rate limiting**, circuit breaking, fan-out routing, customer-facing dashboards, rolling secrets, endpoint-failure notifications (email/Slack), static IPs. License: **Elastic License v2** — source-available, not OSI open source (no offering Convoy itself as a managed service).

### 2.3 Codebase organization

Idiomatic large-Go-service layout: `cmd/` entry points, `api/`, `worker/`, `services/` (business logic), `datastore/` + `database/` + `sql/` (data layer), `queue/`, `cache/`, `config/`, `auth/`, `internal/`, `pkg/`, plus `web/` (Angular), `deploy/`, `monitoring/`. It is a platform codebase, not a library — roughly two dozen top-level packages before you reach delivery logic.

### 2.4 Takeaway for herald

Convoy is the closest Go comparison and the clearest anti-model for weight: two datastores, three service roles, an embedded SPA dashboard, and HA guidance as the default framing. Its features define the ceiling; its deployment footprint defines what herald should refuse to become.

## 3. Hookdeck Outpost — Apache-2.0 Go, "event destinations" scope

### 3.1 Scope and positioning

Outpost (hookdeck/outpost, Go, **Apache-2.0**) generalizes beyond webhooks to **event destinations**: HTTP webhooks plus AWS SQS/S3/EventBridge, GCP Pub/Sub, RabbitMQ, Kafka, and Hookdeck's own gateway. Positioning targets teams adding outbound events for the first time or replacing homegrown systems, with explicit backward compatibility for existing webhook implementations and Standard Webhooks-compatible signatures. It is the open-source arm of Hookdeck's commercial platform.

### 3.2 Infrastructure and deployment

Despite "minimal dependencies" messaging, Outpost requires: **PostgreSQL or ClickHouse** (log storage), **Redis** (entity store), and **one external message queue** (RabbitMQ, AWS SQS, Azure Service Bus, or GCP Pub/Sub) carrying separate delivery and log queues. The binary has three entry points — `api`, `delivery`, `log` — which *can* run in one process, but the docs say single-process mode "is not recommended." So a by-the-book deployment is three services plus three infrastructure systems.

### 3.3 Features

Multi-tenancy, topic-based subscriptions, per-destination content filtering, automatic + manual retries with customizable schedules, event fan-out, JWT-scoped tenant portal, delivery alerts, OpenTelemetry metrics, SDKs (Go/Python/TypeScript). Deliberately delegates advanced processing (transformations etc.) to the user's own infrastructure.

### 3.4 Takeaway for herald

Outpost is the most permissively licensed and the most modern entrant, but its breadth (queue/broker destinations) is exactly what forces three infrastructure dependencies. A webhook-only service does not need a broker abstraction — and dropping it is what makes Postgres-only possible.

## 4. The baseline and the periphery

### 4.1 Stripe — the gold standard of built-in senders

`Stripe-Signature: t=<ts>,v1=<HMAC-SHA256>` over `timestamp.body`, constant-time comparison, secrets rotatable with a 24h dual-validity window. Retries with exponential backoff for up to 3 days in production; manual resend via dashboard (15 days) or CLI (30 days); order explicitly not guaranteed; strong guidance to return 2xx fast and process async. Stripe's docs are the behavioral spec most receivers already assume.

### 4.2 GitHub — the minimal baseline

GitHub **does not automatically retry** failed deliveries at all; 10-second response timeout; manual redelivery via UI and REST API only. Useful floor: even a giant ships webhooks without automatic retries — which is exactly why receivers value senders that do.

### 4.3 Standard Webhooks

A spec (backed by a TSC including Zapier, Twilio, Svix, Kong, ngrok, Supabase) standardizing signature scheme, headers (`webhook-id`, `webhook-timestamp`, `webhook-signature`), and payload conventions, with reference SDKs in nine languages and adopters including OpenAI and Anthropic. Adopting it gives herald interop and free receiver-side verification libraries for near-zero cost.

### 4.4 One-liners

- **Hookdeck Event Gateway (hosted)** — the *receiving*-side complement (ingest, queue, route inbound webhooks); different problem than sending.
- **Trigger.dev** — open-source background-jobs/workflows platform; adjacent (you could hand-roll delivery on it) but not a webhook sender.
- **Webhook Relay** — tunneling/forwarding of inbound webhooks to private networks; receiving-side tooling, not competition.
- **River (riverqueue.com)** — not a webhook product, but the positioning template: "For Go applications built on Postgres... no added services to manage," jobs enqueued transactionally with your data. Herald inherits this via drover.

## 5. Table stakes vs deferrable

### 5.1 Must have (every credible product has it; receivers assume it)

| Feature | Evidence |
|---|---|
| HMAC-SHA256 signing w/ timestamp, rotatable secrets | Svix, Convoy, Outpost, Stripe, Standard Webhooks |
| Automatic retries w/ backoff schedule | Svix (8-step ladder), Convoy, Outpost, Stripe (3 days) |
| Delivery logs: per-attempt status/response/timing, queryable | All four; Stripe's Event Deliveries tab |
| Endpoint management (CRUD, secrets, enable/disable) | All |
| Automatic endpoint disabling with hysteresis | Svix (5-day rule), Convoy (circuit breaker); protects the sender |
| Event types + per-endpoint subscription filtering | Svix event types, Outpost topics, Convoy routing, Stripe event selection |
| Manual replay / bulk recover of failed messages | Svix, Outpost, Stripe resend; GitHub's *only* mechanism |
| Multi-tenancy (application/tenant scoping) | Svix applications, Outpost tenants, Convoy projects |
| Delivery timeout + treating non-2xx (incl. 3xx) as failure | Svix 15s, GitHub 10s, Stripe guidance |

### 5.2 Strongly expected, cheap to include

Per-endpoint rate limiting/throttling (Svix, Convoy both ship it; trivial atop a queue with per-endpoint concurrency), idempotency headers (`webhook-id` from Standard Webhooks gives this for free), SSRF guarding on registered URLs (Svix and Convoy both call it out — an outbound HTTP service that fetches user-supplied URLs must block internal ranges).

### 5.3 Clearly deferrable

Payload transformations (Svix hosted / Convoy differentiators), consumer-facing portal UI (all three; large surface, zero core value for v0.1), non-HTTP destinations — queues/brokers/object storage (this single decision is what costs Outpost its extra infrastructure), static egress IPs / CONNECT proxies (Convoy Egress, Svix Professional), FIFO ordering (Svix Enterprise; Stripe explicitly doesn't guarantee order), broker *sources* (Kafka ingestion), multi-region, SSO/audit/RBAC, dashboards and alerting integrations.

## 6. Differentiation: where "Postgres-only, single binary" sits

### 6.1 Self-hosting complexity, counted in services

| Product | Required infra | Recommended app processes | Minimum systems to operate |
|---|---|---|---|
| Svix OSS | Postgres + Redis (persistent, tuned) | 1 server | 3 |
| Convoy | Postgres 15+ + Redis 6+ | Server + Agent + Egress (×3) | 5+ |
| Outpost | Postgres/ClickHouse + Redis + MQ (RabbitMQ/SQS/ASB/PubSub) | api + delivery + log | 6 |
| **herald (proposed)** | **Postgres** | **1 binary** | **2** |

### 6.2 Is "Postgres is the only dependency" a real differentiator?

Yes, and the market has already validated the argument twice. First, **Convoy's own MongoDB→Postgres migration** shows the incumbents converging on Postgres because self-hosters couldn't operate anything fancier. Second, **River's positioning against Redis-backed queues** ("no added services to manage"; jobs enqueued in the same transaction as your data) is the exact template: every competitor above still needs Redis *as a durable queue* — Svix even requires persistence and `noeviction`, meaning a Redis outage or misconfiguration loses deliveries. Herald built on drover gets transactional enqueue (accept the message and enqueue its delivery jobs in one Postgres transaction — no dual-write window) and one backup/HA/monitoring story. Nobody in the field currently offers "docker run with a DATABASE_URL and nothing else."

### 6.3 What herald gives up

At very high throughput a Postgres queue is the bottleneck sooner than Redis/Kafka; no static-IP story; no portal. These are acceptable: the target user self-hosts precisely because their volume is modest and their ops budget is one database.

## Comparison table

| | Svix (OSS server) | Convoy | Hookdeck Outpost | Stripe/GitHub (built-in) | herald v0.1 (proposed) |
|---|---|---|---|---|---|
| Language | Rust | Go (+ Angular UI) | Go | n/a | Go |
| License | MIT | Elastic License v2 (source-available) | Apache-2.0 | proprietary | TBD (OSI) |
| Required infra | Postgres + Redis (persistent) | Postgres 15+ + Redis 6+ | Postgres or ClickHouse + Redis + MQ | n/a | Postgres only |
| App processes (recommended) | 1 | 3 roles (Egress ×3) | 3 (single-process "not recommended") | n/a | 1 |
| Signing | HMAC + Ed25519 | HMAC (advanced schemes) | Standard Webhooks-compatible | Stripe: HMAC-SHA256 + ts; GitHub: HMAC | Standard Webhooks HMAC-SHA256 |
| Retries | 8-step ladder | constant + exponential, batch | auto + custom schedules | Stripe 3d; GitHub none | fixed ladder via drover |
| Auto endpoint disable | Yes (5-day + hysteresis) | Circuit breaker | Alerts + disable | Stripe: on prolonged failure | Yes |
| Event types / filtering | Yes (+ channels) | Yes (routing, fan-out) | Topics + content filtering | Yes | Event types, per-endpoint filter |
| Manual replay | Per-msg + bulk recover + replay-missing | Batch retries | Manual retries | Resend (15–30d) / redeliver API | Per-message + bulk since-date |
| Per-endpoint rate limit | Throttling | Yes | Delivery config | n/a | Yes (worker concurrency cap) |
| Portal UI | Hosted feature | Yes (dashboards) | JWT portal | Dashboard | No — API only |
| Non-HTTP destinations | Hosted enterprise | Broker ingest | SQS/S3/PubSub/RabbitMQ/Kafka/EventBridge | No | No |
| Transformations | Hosted | Yes | Delegated to user | No | No |

## Gap analysis

- **No product occupies "Postgres-only, single binary."** Svix OSS is closest in process count but demands durable Redis and is the deliberately un-optimized sibling of a hosted product; Convoy and Outpost both assume multi-service deployments. The two-systems operating footprint (herald + Postgres) is an open position.
- **Transactional integrity is unclaimed.** All three queue through Redis or an external MQ, creating an accept-then-enqueue dual-write window. Drover's Postgres queue lets herald claim "if the API returned 202, the delivery jobs exist" as a mechanism-level guarantee, mirroring River's pitch.
- **License gap.** Convoy is ELv2; Svix OSS is MIT but strategically secondary; Outpost is Apache-2.0 but broad-scope. A genuinely OSI-licensed, deliberately narrow tool has room.
- **Auditability gap.** Convoy's ~25 top-level packages and Outpost's three-service topology are hard to audit; "small enough to read in an afternoon" is a real property none of them can offer.
- **Herald's gaps to accept:** no portal, no static IPs, no broker destinations, throughput ceiling of a Postgres queue, and no brand recognition — mitigated by Standard Webhooks compatibility so receivers need nothing herald-specific.

## Options

**Option A — Webhook-sender core, Postgres-only, API-first (the narrow cut).** ★ RECOMMENDED. In: HTTP API to create applications/endpoints/event-types and publish messages; fan-out to subscribed endpoints; Standard Webhooks signing (HMAC-SHA256, `webhook-id`/`webhook-timestamp` headers, rotatable secrets); fixed retry ladder via drover with per-attempt logs (status, response excerpt, latency) queryable over the API; automatic endpoint disabling with Svix-style hysteresis; manual per-message replay and bulk recover-since-date; per-endpoint delivery concurrency/rate cap; SSRF guard on endpoint URLs; delivery timeout with non-2xx (incl. 3xx) as failure; single binary, `DATABASE_URL` the only required config. Out (walls): portal/dashboard UI, transformations, non-HTTP destinations, broker sources, static IPs, FIFO ordering, multi-region, SSO/RBAC, alerting integrations. *Why:* this is exactly the table-stakes set of §5.1 — nothing a receiver assumes is missing — while every cut item is the direct cause of a competitor's extra service or codebase weight; it maximizes the two real differentiators (two-system ops, transactional enqueue) and dogfoods drover end to end. *Why not:* no UI means demos are curl-driven and less visual; teams wanting a customer portal must look elsewhere at v0.1.

**Option B — Add a minimal read-only dashboard to Option A.** *Why:* visibility is the most-cited webhook pain, and a small server-rendered status page demos well. *Why not:* even a "minimal" UI is a second product surface (auth, assets, design) that grows the audit area and delays the core; queryable logs over the API deliver the same information, and a UI can ship as v0.2 without any schema change.

**Option C — Follow Outpost into pluggable event destinations (webhooks + SQS/PubSub).** *Why:* broader market, modern "event destinations" framing. *Why not:* it is precisely the decision that forces broker SDKs, per-destination config models, and eventually the multi-service topology herald exists to avoid; it also dilutes the auditable-core claim. Rejected for v0.1 and skeptically viewed even later.

**Option D — Bidirectional (add inbound webhook ingestion/receiving).** *Why:* Hookdeck-style receiving is a real adjacent need. *Why not:* receiving is a different product (queuing inbound, verification, routing to internal consumers) with different tenancy; bundling both halves at v0.1 guarantees neither is done well.

**Positioning statement (recommended):** Herald is a self-hosted webhook delivery service for teams that need to send signed, reliably retried webhooks to their customers without adopting a platform: a single Go binary whose only dependency is the Postgres you already run, where accepting a message and scheduling its deliveries happen in one transaction, with Standard Webhooks-compatible signatures, automatic endpoint disabling, and queryable delivery logs — a core small enough to audit in an afternoon, built on the drover task queue.

## Sources

- https://github.com/svix/svix-webhooks (accessed 2026-07-25) — Svix OSS server README: Rust stack, MIT license, Postgres + persistent Redis requirement, OSS-vs-hosted framing.
- https://docs.svix.com/ (accessed 2026-07-25) — Svix docs index: domain model (application/endpoint/message/attempt/event type), throttling, channels, replay.
- https://docs.svix.com/retries (accessed 2026-07-25) — retry ladder, 15s timeout, 5-day auto-disable with hysteresis, Recover Failed / Replay Missing.
- https://www.svix.com/pricing/ (accessed 2026-07-25) — Free/Professional/Enterprise split; enterprise-only features (FIFO, broker/DB endpoints, custom retry schedules, SSO).
- https://github.com/frain-dev/convoy (accessed 2026-07-25) — Convoy README: Go + Angular, ELv2 license, feature list (signing, rate limiting, circuit breaking, batch retries).
- https://getconvoy.io/docs/deployment/architecture.md (accessed 2026-07-25) — Server/Agent/Egress roles, Postgres 15+ and Redis 6+ requirements, HA deployment guidance.
- https://api.github.com/repos/frain-dev/convoy/contents/ (accessed 2026-07-25) — top-level package layout of the Convoy codebase.
- https://www.getconvoy.io/blog/convoy-0.9 (accessed 2026-07-25) — v0.9 MongoDB→Postgres migration rationale (replica-set burden for self-hosters, migration tooling, query speed).
- https://github.com/hookdeck/outpost (accessed 2026-07-25) — Outpost README: Go, Apache-2.0, event-destination types (SQS, S3, EventBridge, Pub/Sub, RabbitMQ, Kafka).
- https://hookdeck.com/docs/outpost (accessed 2026-07-25) — concepts (tenants, destinations, topics), features, managed-vs-self-hosted positioning.
- https://hookdeck.com/docs/outpost/guides/deployment (accessed 2026-07-25) — infra: Postgres or ClickHouse + Redis + one MQ; api/delivery/log entry points; single-process "not recommended."
- https://www.standardwebhooks.com/ (accessed 2026-07-25) — spec scope, TSC membership (Zapier, Twilio, Svix, Kong…), nine reference SDKs, adopters.
- https://docs.stripe.com/webhooks (accessed 2026-07-25) — Stripe-Signature scheme, 3-day retry policy, resend windows, out-of-order delivery, receiver best practices.
- https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries (accessed 2026-07-25) — GitHub does not auto-retry; 10s timeout; manual redelivery via UI/API.
- https://riverqueue.com/ (accessed 2026-07-25) — River's Postgres-only positioning: "no added services to manage," transactional enqueueing guarantee.

**Unverified claims flagged:** the §4.4 one-liners on Trigger.dev, Webhook Relay, and Hookdeck's hosted Event Gateway are from general knowledge and were not verified against primary sources in this session; "nobody currently offers docker-run-with-a-DATABASE_URL" (§6.2) is an inference from the three surveyed products' documented requirements, not an exhaustive market scan; Convoy's exact package count ("~25 top-level packages") is an approximation from the GitHub contents listing; Svix Enterprise feature boundaries were read from the pricing page and may lag the actual product; Stripe's automatic disabling of failing endpoints is documented only loosely (docs confirm behavior around disabled destinations but not a precise disable rule), so the comparison-table cell "on prolonged failure" reflects widely reported behavior rather than an explicit primary-source statement; Outpost's Redis-cluster support and the claim that ClickHouse fully substitutes Postgres for all storage roles were taken from summarized docs and deserve re-verification before citing in public material.
