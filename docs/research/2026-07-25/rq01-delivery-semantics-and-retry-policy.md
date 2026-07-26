# RQ01 — Delivery semantics and retry policy

Research date: 2026-07-25. Scope: what delivery guarantee a webhook sender can honestly promise, the exact retry schedules, success/failure classification rules, timeout budgets, endpoint-disabling policies, ordering semantics, and replay features used by major webhook providers (Stripe, GitHub, Shopify, Svix, Convoy, Hookdeck) and the Standard Webhooks spec — and what herald v0.1 should adopt.

## 1. At-least-once delivery is the industry contract

### 1.1 Why nobody promises exactly-once

Exactly-once delivery over an unreliable network is not a feature a vendor can ship; it reduces to the Two Generals problem. The failure mode is concrete: the sender POSTs an event, the receiver processes it, and the connection dies before the 2xx response reaches the sender. Svix's delivery-guarantees material states the dilemma precisely: "the provider does not know you succeeded. If they retry, you get a duplicate (violating exactly-once). If they do not retry, you might have failed (also violating exactly-once)... Without reading your server's internal state, the provider cannot make the right choice." Every sender that retries therefore delivers at-least-once, and every sender that does not retry delivers at-most-once. There is no third option.

At-least-once is the dominant choice because, in Svix's words, "losing events is usually worse than handling duplicates." Stripe documents the consequence directly: "Occasionally, webhook endpoints can receive the same event more than once" (Portuguese-locale rendering of docs.stripe.com/webhooks; same content).

### 1.2 What receivers are told to do

The mitigation is uniform across the industry: stable event IDs plus idempotent consumption.

- **Stripe**: "To protect against receiving duplicate events, record the Event IDs that you have processed and do not process already-recorded events." Stripe also warns that in some cases two distinct `Event` objects are generated for the same underlying change, so it recommends deduplicating on `data.object` ID plus `event.type` as a second layer.
- **GitHub**: "Use the `X-GitHub-Delivery` header to ensure that each delivery is unique per event." On manual redelivery the header value is preserved: "If you request a redelivery, the `X-GitHub-Delivery` header will be the same as in the original delivery" — i.e., the ID identifies the event, not the attempt.
- **Shopify**: recommends using `X-Shopify-Webhook-Id` to "ignore duplicate deliveries," and goes further: because "webhook delivery isn't always guaranteed," it recommends "reconciliation jobs to periodically fetch data from Shopify" as redundancy.
- **Standard Webhooks spec**: the `webhook-id` header "remains the same no matter how many times a webhook that has failed is retried," and consumers should "use the `webhook-id` header as an idempotency key to prevent accidentally processing the same webhook more than once (e.g. save the IDs in redis for 5 minutes)."

### 1.3 Mechanism consequence for herald

The sender's half of the contract is: (a) mint one stable message ID at ingestion, (b) send that same ID (and a creation timestamp) on every attempt, (c) retry until acknowledged or the policy gives up, and (d) document to tenants' receivers that duplicates are possible and consumption must be idempotent. Herald cannot do the receiver's half; it can only make it possible.

## 2. Retry schedules used by major providers

### 2.1 Stripe

"Stripe attempts to deliver events to your destination for up to three days with an exponential back off in live mode." In sandbox/test mode, retries happen only "three times over a few hours." Stripe does not publish the exact live-mode interval table — only the shape (exponential) and the window (72 h). The next scheduled retry for a given event is visible in the Dashboard.

### 2.2 GitHub

GitHub is the notable outlier: "GitHub does not automatically redeliver failed deliveries." Zero automatic retries. The documented remediation is manual: redeliver from the UI, or "write a script that checks for failed deliveries and attempts to redeliver any that failed" via the REST API on a schedule. This shifts the entire reliability burden to the receiver and is widely worked around by consumers running exactly such cron scripts.

### 2.3 Shopify

Shopify retries a failed delivery "up to eight times in a four-hour period" (increasing intervals), after a 1-second connection timeout and 5-second total response deadline. After sustained failure — "multiple failures in a 24-hour period" per the troubleshooting docs; commonly reported as 8 consecutive failed deliveries for Admin-API-created subscriptions — the webhook subscription itself is removed, and warning emails go to the app's emergency developer email address. Shopify's changelog documents that the retry mechanism was updated over time (older third-party accounts describe 19 attempts over 48 h), so the 8-over-4h numbers should be treated as the current regime.

### 2.4 Svix

Svix publishes its exact schedule: retries follow "Immediately, 5 seconds, 5 minutes, 30 minutes, 2 hours, 5 hours, 10 hours, 10 hours (in addition to the previous)" — 8 total attempts. Summing the intervals, the final attempt lands roughly 27.5 hours after the first. The schedule is exponential-shaped but published as a fixed table, which makes it documentable and lets receivers predict when the next attempt will arrive.

### 2.5 Convoy

Convoy (open-source webhook gateway in Go, the closest architectural neighbor to herald) supports two configurable strategies: linear (fixed interval, e.g. 1 hour between attempts) and exponential backoff computed as `min(delaySeconds * 2^attempt, maxRetrySeconds) + jitter`, with defaults of base delay 20 s, cap 7200 s (2 h), jitter ±10%, and a default retry limit of 20 attempts. Its docs note the retry behavior "applies to subscriptions using the at least once delivery mode (the default)."

### 2.6 Hookdeck

Hookdeck exposes retry policy as user configuration rather than a fixed schedule: linear ("retries occur at regular intervals") or exponential ("each retry is delayed twice as long as the previous (1 hour, 2 hours, 4 hours, etc.)"), with configurable interval, count, and which response status codes trigger a retry (ranges like "500-599", comparisons like ">=500"). Hard limits: "Events are limited to 50 automatic retries"; a `Retry-After`-driven retry "can be scheduled up to 7 days in the future."

### 2.7 Standard Webhooks spec

The spec's recommendation matches the Stripe/Svix shape: "It's recommended to retry delivery following a retry schedule spanning multiple days, with an exponential backoff. It's recommended to also add some level of random jitter to retries."

## 3. Backoff shape: exponential plus jitter

### 3.1 Why exponential

Failures cluster: a receiver that just returned 500 will very likely return 500 again in 5 seconds, and quite possibly succeed in 2 hours (deploys, incidents, DNS changes all resolve on minutes-to-hours timescales). Exponential spacing concentrates attempts where recovery is cheap and fast (transient blips resolved by the first one or two retries) while spending only a handful of attempts covering the long tail of multi-hour outages. This is why every published schedule (Stripe, Svix, Convoy default, Standard Webhooks) is front-loaded with second/minute-scale gaps and back-loaded with hour-scale gaps.

### 3.2 Why jitter

Pure exponential backoff synchronizes clients. AWS's analysis (the canonical treatment) shows that with deterministic backoff "there are still clusters of calls. Instead of reducing the number of clients competing in every round, we've just introduced times when no client is competing." For a webhook sender the "clients" are the queued deliveries for one endpoint: if an endpoint is down for an hour, hundreds of messages fail at nearly the same moment and — without jitter — all retry at nearly the same moment, hammering the endpoint exactly when it comes back (thundering herd, self-inflicted). With jitter "the gaps are gone, and beyond the initial spike, there's an approximately constant rate of calls"; at 100 contending clients AWS measured total work "reduced by more than half" versus non-jittered backoff. Full jitter (`sleep = random(0, min(cap, base * 2^attempt))`) was the best performer; even the cheap version (Convoy's ±10% multiplicative jitter) breaks synchronization adequately for webhook-scale traffic.

### 3.3 Fixed table vs. computed formula

Two implementation idioms exist: a published fixed table with jitter applied per attempt (Svix, Stripe-shaped), or a computed formula `base * 2^n` with cap and jitter (Convoy, Hookdeck). The fixed table is easier to document ("attempt 5 happens ~2 hours after attempt 4"), easier to test, and maps directly onto a task queue's `run_at` column; the formula is easier to make tenant-configurable. Providers whose schedule is part of their public contract (Stripe, Svix) use the table.

## 4. What counts as success and failure

### 4.1 Status-code policy

The consensus rule, stated verbatim in the Standard Webhooks spec: "A webhook delivery is considered successful if it was responded to with a `2xx` status code (status codes 200-299)." Everything else is a failure. The interesting edge cases:

- **3xx redirects — do not follow, count as failure.** Stripe: "We consider redirect responses to webhook requests as failures" (a `302` row appears in its error table alongside 4xx/5xx). Standard Webhooks agrees and gives the mechanism-level reason: "Following redirects causes unnecessary load," and following them would also re-POST a signed body to a URL the tenant never registered — a signature-scope and SSRF-adjacent hazard. The URL on file is the contract; if the receiver moved, the tenant updates the endpoint.
- **410 Gone — special-cased as "stop entirely."** Standard Webhooks: on `410 Gone`, disable the endpoint entirely. It is the one status code that is an explicit message from the receiver to the sender ("this resource is intentionally gone; do not retry"), so treating it like a generic 4xx wastes a full retry window per message.
- **429 / 502 / 504 — back-pressure signals.** Standard Webhooks recommends throttling delivery to the endpoint on `429 Too Many Requests` and treating `502`/`504` as overload indicators. Hookdeck additionally honors `Retry-After` (scheduling a retry "up to 7 days in the future").
- **4xx generally** — a failure like any other; providers still retry it on the normal schedule (the receiver may fix a bug), with 410 as the only documented early-exit.
- **Transport-level errors** — TLS failure, connection refused/unreachable, and timeout are all failures in Stripe's error table ("The destination server took too long to respond to the webhook request").

### 4.2 Timeout budgets

Published per-attempt budgets:

| Provider | Connection timeout | Total response timeout |
|---|---|---|
| Shopify | 1 s | 5 s |
| GitHub | — | 10 s ("respond with a 2XX response within 10 seconds") |
| Svix | — | 15 s ("a reasonable time-frame (15s with Svix)") |
| Standard Webhooks (recommendation) | — | "somewhere between 15 and 30s" |
| Hookdeck | — | 60 s |

The universal companion advice is ack-then-process: Stripe — "return quickly a success status code (2xx) before any complex logic that could cause a timeout"; GitHub — "you may want to set up a queue to process webhook payloads asynchronously." A sender's timeout budget and its receivers' async-processing discipline are two halves of the same mechanism: the shorter the budget, the more firmly receivers are pushed toward queue-and-ack, which in turn makes deliveries fast and cheap for the sender.

## 5. Endpoint health: automatic disabling

### 5.1 Why disable at all

An endpoint that has been failing for days turns the delivery queue into a treadmill: every new message burns a full retry schedule (network, worker time, log rows) with near-zero success probability, and bulk-recovery-after-fix is a better repair path anyway. Disabling is queue hygiene, not punishment — but it converts "late" into "lost" for new messages, so every provider that does it pairs it with notification.

### 5.2 Svix (the most precisely documented policy)

Svix disables an endpoint after all attempts to it fail "for a period of 5 days," with an anti-flap guard: "The clock only starts after multiple deliveries failed within a 24 hour span, with at least 12 hours difference between the first and the last failure" — i.e., one bad hour or a single failing message cannot start the countdown. On disabling, an `EndpointDisabledEvent` operational webhook notifies the account owner, and the auto-disable behavior can be toggled off entirely. Recovery is manual re-enable plus bulk recovery of missed messages (section 7).

### 5.3 Stripe

Stripe's primary docs confirm the 72-hour per-event retry window and that Stripe emails you when an endpoint is failing; its support content confirms disable-related emails exist ("Stripe already sends these automatically when your endpoint is failing or gets disabled"). Third-party accounts consistently describe automatic disabling after roughly 3 days of uniform failure, but the current docs.stripe.com/webhooks page does not state an explicit auto-disable threshold — it only describes behavior *when* a destination "has been disabled or deleted" (retries for already-queued events are then suppressed; if you disable and re-enable before a retry fires, the retry still happens). Treat "Stripe auto-disables after N days" as unverified (flagged below).

### 5.4 Shopify (the harshest policy)

Shopify does not disable — it deletes. "After multiple failures in a 24-hour period, the webhook subscription is removed" (Admin-API-created subscriptions), with warning emails to the app's emergency developer address first. The app must detect the deletion and re-create the subscription; Shopify's own guidance is to run a check that "fetches all the existing subscriptions and creates only the ones that you need." Deleting configuration rather than pausing it is widely considered a footgun (an ecosystem of monitoring tools exists specifically to catch it).

### 5.5 GitHub and the spec

GitHub's consumer docs describe no automatic disabling for repository/org webhooks — consistent with GitHub having no automatic retries: a failing endpoint costs GitHub one attempt per event, so there is no treadmill to stop. The Standard Webhooks spec makes disabling a recommendation with a notification requirement: when delivery "fail[s] consistently over a long period of time... notify the consumers using other channels (e.g. email), and [it] is recommended to disable future delivery to the endpoint."

## 6. Ordering

### 6.1 What providers actually guarantee: nothing

- Stripe: "Stripe does not guarantee delivery of events in the order they were generated" (e.g., `invoice.paid` may arrive before `invoice.created`). Recommended posture: "Ensure that your event destination does not depend on receiving events in a specific order... You can also use the API to retrieve missing objects."
- Shopify: "Shopify doesn't guarantee ordering within a topic, or across different topics for the same resource"; use `X-Shopify-Triggered-At` / `updated_at` timestamps to order events yourself.
- GitHub, Standard Webhooks: silent on ordering — no guarantee offered.
- Svix (default endpoints): no ordering guarantee, with a whole blog post explaining why.

### 6.2 Why strict ordering conflicts with retries

The conflict is structural. If message A fails and enters its backoff schedule while message B succeeds immediately, either B waits for A (head-of-line blocking: one dead message stalls the whole endpoint for the length of the retry window) or B overtakes A (ordering broken). Svix's analysis adds a second, less obvious point: ordering the *sends* doesn't order the *processing* anyway — "even if you send `first` first, it will in practice be processed after `second`" when the receiver handles requests concurrently, and serializing sends per endpoint means "our webhook delivery will essentially be limited to one webhook per second, which is terrible." Worse, the best-practice receiver (ack immediately, process from a queue — section 4.2) has explicitly given up processing-order information at the moment it acks. Svix's FIFO endpoints exist as an opt-in that accepts exactly this cost: batched, acknowledged-in-order delivery where "a single slow or failing endpoint blocks all subsequent deliveries to that destination."

### 6.3 What "best-effort ordering" means in practice

Dispatch messages in creation order when nothing has failed, let retries overtake, and give receivers the tools to reorder or ignore: a creation timestamp (and/or per-resource version counter) in every payload so a receiver "can check whether this event is newer or older" than what it has, and thin payloads that push receivers to fetch current state from the API rather than replaying event history. That is the whole contract; providers deliberately do not promise more.

## 7. Manual redelivery and replay

### 7.1 Single-message retry

- Stripe: resend an individual event from the Dashboard "up to 15 days after event creation," or via the CLI "up to 30 days after event creation."
- GitHub: redeliver from the UI or via dedicated REST endpoints (`.../deliveries/{id}/attempts`); this is GitHub's *only* retry mechanism, and redeliveries keep the original `X-GitHub-Delivery` ID.
- Hookdeck: individual events retryable from dashboard or API "as many times as you like"; a successful manual retry cancels "any scheduled automatic retries for that event."
- Svix: resend individual messages from the application portal or API.

### 7.2 Bulk replay

- Svix: "bulk recovery of all failed messages from a specified date" (the standard repair after a fixed outage or a re-enabled endpoint), plus replay of "messages never attempted to an endpoint" — which covers the endpoint-added-later case.
- Hookdeck: bulk retries over filtered event lists or API queries.
- Convoy: "batch retries for endpoints [that] consecutively failed to process retried events."

The pattern: automatic retries handle transient failure; time-range bulk replay handles the systematic failure ("our handler was broken from 09:00 to 11:30") that no automatic schedule can fix. A delivery log queryable by endpoint, status, and time range is the substrate both features share — replay is just "select failed attempts in range, re-enqueue."

## Comparison table

| Provider | Auto retries | Schedule | Total window | Per-attempt timeout | Success rule | Redirects | Auto-disable policy | Manual replay |
|---|---|---|---|---|---|---|---|---|
| **Stripe** | Yes | Exponential backoff (intervals unpublished) | Up to 3 days (live); 3 retries over a few hours (sandbox) | Not published ("timeout" is a failure class) | 2xx only | Failure, not followed | Email on failing endpoint; auto-disable threshold not stated in current docs | Dashboard ≤15 days, CLI ≤30 days, per event |
| **GitHub** | **No** | — | — | 10 s | 2xx within 10 s | Not documented | None documented | UI + REST API redelivery (same delivery ID) |
| **Shopify** | Yes | 8 retries, increasing intervals | 4 hours | 1 s connect / 5 s total | 2xx | Not documented | Subscription **deleted** after multiple failures in 24 h; warning emails first | Not offered; docs recommend reconciliation polling |
| **Svix** | Yes | Fixed table: 0, 5 s, 5 m, 30 m, 2 h, 5 h, 10 h, 10 h (8 attempts) | ~28 hours | 15 s | 2xx | Failure | Disabled after 5 days of continuous failure (clock needs ≥12 h first-to-last failure spread in 24 h); `EndpointDisabledEvent` webhook; opt-out | Single resend + bulk recovery from date + replay never-attempted |
| **Convoy** | Yes | Linear (e.g. 1 h fixed) or `min(20s * 2^n, 7200s) ± 10%` jitter | Depends on config (default 20 attempts) | Configurable | 2xx (at-least-once mode default) | Not documented | Disables endpoint after consecutive failures + notification | Batch retries |
| **Hookdeck** | Yes | Linear or exponential (1 h, 2 h, 4 h, ...), user-configured; status-code rules configurable | ≤50 attempts; `Retry-After` up to 7 days | 60 s | Configurable (e.g. retry on >=500) | Not documented | Not documented (delivery-tooling product) | Single + bulk (filtered), unlimited manual |
| **Standard Webhooks (spec)** | Recommended | Exponential + jitter | "multiple days" recommended | 15–30 s recommended | 2xx | Failure ("unnecessary load") | Recommended after consistent long-term failure, with out-of-band notification; 410 → disable immediately | Not specified |

## Gap analysis

- **Stripe's live-mode interval table is unpublished.** "Exponential over 3 days" is the entire public spec; exact attempt counts/intervals cannot be cited from a primary source.
- **Stripe's auto-disable threshold is not in the current primary docs.** Email notification on failure is documented; "disables after ~3 days of uniform failure" appears only in support-adjacent and third-party accounts. Flagged as unverified.
- **Shopify's exact deletion trigger is fuzzy in primary docs** ("multiple failures in a 24-hour period"); the widely cited "8 consecutive failures" figure comes from third-party summaries, and older sources describe a superseded 19-attempt/48 h regime (Shopify's changelog confirms the mechanism changed).
- **Convoy's circuit-breaker thresholds** (how many consecutive failures trip disabling) were not found on the fetched pages; only the existence of the disable-plus-notify behavior is confirmed.
- **Redirect handling is undocumented for GitHub, Shopify, and Hookdeck**; only Stripe and the Standard Webhooks spec state the fail-don't-follow rule explicitly.
- **No provider publishes jitter parameters except Convoy (±10%)**; Stripe/Svix presumably jitter but do not document it.
- **Ordering guarantees under retry are documented by argument (Svix blog), not by spec** — no provider formally defines "best-effort ordering"; it is a de facto term.

## Options

**Option A — Svix-style fixed schedule, Standard-Webhooks status semantics, Svix-style disabling. ★ RECOMMENDED**

Concrete policy for herald v0.1:

- *Schedule*: 8 attempts per message — immediate, then +5 s, +5 m, +30 m, +2 h, +5 h, +10 h, +10 h (~28 h total window), with full-jitter randomization of each delay up to its nominal value floor-clamped to ~50% (equal-jitter style), stored as a `run_at` computed at failure time. Maps 1:1 onto drover's scheduled-task model: each failed attempt schedules the next task.
- *Timeout budget*: 15 s total per attempt, 5 s connect. Long enough for a slow-but-honest receiver, short enough to keep workers cheap; matches Svix exactly and sits at the bottom of the spec's 15–30 s band.
- *Success/failure*: 2xx = success; everything else = failure. Never follow redirects (3xx = failure). Special-case `410 Gone`: disable the endpoint immediately and stop all pending deliveries to it. Record status code, latency, and truncated response body per attempt in the delivery log.
- *Disabling*: automatic disable when every delivery to an endpoint has failed for 5 consecutive days, with the Svix anti-flap guard (countdown starts only once failures within a 24 h span are at least 12 h apart first-to-last). Disable emits an operational event/log row visible to the tenant; re-enable is manual (API/dashboard), paired with bulk replay. v0.1 ships manual re-enable only — no auto-recovery probing.
- *Replay*: single-message redeliver (same message ID) in v0.1; bulk replay by endpoint + time range as the immediate follow-up, both reading from the same delivery-log substrate.

Why: every element is copied from a documented, battle-tested policy (Svix schedule and disable policy, Standard Webhooks status semantics, AWS jitter analysis), so herald's docs can cite prior art for each number; a fixed table is trivially testable and publishable as part of herald's tenant-facing contract; the ~28 h window fits inside Stripe's 3-day norm while keeping worst-case queue residency bounded for a self-hosted deployment. Why not: the window is shorter than Stripe's 72 h, so a receiver outage longer than ~28 h converts to "lost until replayed" — acceptable in v0.1 because bulk replay is the designed repair path, and the schedule constants can be lengthened without schema changes.

**Option B — Computed exponential formula, Convoy-style (`min(base * 2^n, cap) + jitter`, tenant-configurable).** Rejected for v0.1: configurability multiplies the test matrix and support surface (per-tenant schedules, caps, and limits) before there is a second tenant to need it; a computed formula with a 2 h cap also degenerates into a linear tail (Convoy's default spends attempts 8–20 at a flat 2 h spacing), spending many attempts for little added coverage. Revisit as a v0.2 override on top of Option A's defaults.

**Option C — Aggressive short window, Shopify-style (8 attempts / 4 h, then delete the endpoint).** Rejected: a 4 h window converts any overnight receiver outage into total event loss, and deleting subscription state (rather than pausing it) is the most complained-about behavior in this survey — it forces every tenant to build re-subscription reconciliation. Herald's philosophy (durable Postgres queue, queryable logs) makes the short window pointless: retention is cheap.

**Option D — No automatic retries, GitHub-style (manual redelivery only).** Rejected: it exports the reliability problem to every tenant (GitHub's own docs tell consumers to build cron-driven redelivery scripts), which is the opposite of herald's value proposition; GitHub can afford it only because its consumers are developers with API access and its event volume makes per-event retry state expensive at a scale herald does not have.

## Sources

- https://docs.stripe.com/webhooks (accessed 2026-07-25) — Stripe retry window (3 days live / 3 retries sandbox), duplicate-event warning, event-ID idempotency advice, no-ordering guarantee, 2xx success rule, 3xx-as-failure table, timeout-as-failure, ack-before-processing advice, behavior when a destination is disabled/deleted.
- https://support.stripe.com/questions/troubleshooting-webhook-delivery-issues (accessed 2026-07-25) — confirms Stripe retries "several times"; used to corroborate failing-endpoint email notifications.
- https://docs.github.com/en/webhooks/using-webhooks/handling-failed-webhook-deliveries (accessed 2026-07-25) — GitHub does not auto-redeliver; 10 s failure threshold; manual/scripted redelivery via REST API.
- https://docs.github.com/en/webhooks/using-webhooks/best-practices-for-using-webhooks (accessed 2026-07-25) — 10 s response requirement, async-queue advice, `X-GitHub-Delivery` dedup, redelivery keeps the same delivery ID.
- https://shopify.dev/docs/apps/build/webhooks (accessed 2026-07-25) — no ordering within/across topics, `X-Shopify-Webhook-Id` dedup, `X-Shopify-Triggered-At` timestamps, reconciliation-job advice.
- https://shopify.dev/docs/apps/build/webhooks/troubleshooting-webhooks (accessed 2026-07-25) — 8 retries over 4 hours, 5 s response deadline, subscription removal after multiple failures in 24 h, re-subscription guidance.
- https://shopify.dev/changelog/updates-to-webhook-retry-mechanism (accessed 2026-07-25) — evidence the retry regime changed over time (why older 19-attempt figures circulate).
- https://docs.svix.com/retries (accessed 2026-07-25) — exact Svix schedule (0, 5 s, 5 m, 30 m, 2 h, 5 h, 10 h, 10 h), 15 s timeout, 2xx rule, 5-day disable policy with 24 h/12 h anti-flap guard, `EndpointDisabledEvent`, opt-out, single resend / bulk recovery / replay-never-attempted.
- https://raw.githubusercontent.com/standard-webhooks/standard-webhooks/main/spec/standard-webhooks.md (accessed 2026-07-25) — `webhook-id` idempotency-key guidance, exponential-backoff-plus-jitter recommendation over multiple days, 2xx success definition, 3xx-as-failure rationale, 410-disable and 429/502/504 throttling guidance, 15–30 s timeout recommendation, disable-plus-notify recommendation.
- https://www.getconvoy.io/docs/webhook-guides/webhook-retries (accessed 2026-07-25) — Convoy strategies (linear 1 h; `min(20s * 2^n, 7200s) ± 10%` jitter), default 20-attempt limit, at-least-once default mode.
- https://github.com/frain-dev/convoy (accessed 2026-07-25, via search excerpt) — Convoy disables endpoints after consecutive failures and sends a notification; batch retries.
- https://hookdeck.com/docs/retries (accessed 2026-07-25) — Hookdeck linear/exponential config, 50-attempt cap, 60 s timeout, status-code retry rules, `Retry-After` up to 7 days, single/bulk/manual retries, manual retry cancels scheduled automatic ones.
- https://aws.amazon.com/blogs/architecture/exponential-backoff-and-jitter/ (accessed 2026-07-25) — thundering-herd mechanism under deterministic backoff; full/equal/decorrelated jitter definitions; >50% work reduction at 100 clients; full jitter as best default.
- https://www.svix.com/blog/guaranteeing-webhook-ordering/ (accessed 2026-07-25) — why send-order does not imply processing-order, head-of-line blocking cost, thin-payload + timestamp/version-counter recommendation, FIFO endpoints as explicit opt-in.
- https://www.svix.com/resources/webhook-university/reliability/webhook-delivery-guarantees/ (accessed 2026-07-25) — Two-Generals argument for the impossibility of exactly-once, at-least-once as the dominant model, idempotent-handler guidance, per-partition ordering note.
- https://hookdeck.com/webhooks/guides/webhook-delivery-guarantees (accessed 2026-07-25, via search excerpt) — corroborates at-least-once as the industry norm and "exactly-once = at-least-once + idempotent processing" framing.

**Unverified claims flagged:** (1) Stripe automatically disabling an endpoint after ~3 days of uniform failure — supported by Stripe support-adjacent content and multiple third-party accounts, but no explicit auto-disable threshold appears on the current docs.stripe.com/webhooks page, which documents only retry suppression for already-disabled destinations and failure-notification emails. (2) Shopify deleting a subscription specifically after "8 consecutive failures" — primary docs say only "multiple failures in a 24-hour period"; the count of 8 (and the 1 s connection timeout) comes from third-party summaries and search aggregation, and the superseded "19 attempts over 48 h" figure is third-party only. (3) Svix's total retry window being "approximately 32 hours" appeared in an intermediate summary; the interval sum from the primary schedule is ~27.5 h, and this document uses ~28 h — Svix itself publishes only the interval list, not a total. (4) Convoy's exact circuit-breaker threshold (number of consecutive failures before disabling) — the disable-and-notify behavior is confirmed from the project README, but no numeric threshold was found in the fetched documentation. (5) The claim that Stripe and Svix apply jitter to their published schedules — plausible and consistent with the Standard Webhooks recommendation both are associated with, but not stated in either provider's retry documentation.
