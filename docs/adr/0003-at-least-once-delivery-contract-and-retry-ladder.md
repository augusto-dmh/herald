# ADR-0003: At-least-once delivery with a fixed 8-attempt retry ladder

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: reliability, delivery

## Context and Problem Statement

Herald must commit to a delivery contract — what "delivered" means, how failures are retried, and when an endpoint is given up on — because receivers build their consumption logic against it and it cannot change quietly later.

## Decision Drivers

- Exactly-once delivery over HTTP is impossible; the sender cannot distinguish "processed, ack lost" from "not processed".
- Published provider practice (Svix's fixed ladder, Stripe's 3-day window) sets receiver expectations herald should match, not surprise.
- Retries structurally conflict with ordering guarantees; promising FIFO would be a lie.
- Dead endpoints must not consume delivery capacity forever.

## Considered Options

**At-least-once with a Svix-shaped fixed ladder** · **At-least-once with unbounded exponential backoff (Stripe-shaped)** · **No automatic retries (GitHub-shaped)**

## Decision Outcome

Chosen: **At-least-once with a Svix-shaped fixed ladder**, with:

- **Contract**: at-least-once per (message, endpoint); receivers deduplicate on the stable message ID delivered in the signed headers. No ordering guarantee; messages are dispatched in creation order, best-effort.
- **Ladder**: attempts at 0s, 5s, 5m, 30m, 2h, 5h, 10h, 10h (8 total, ~28h span), each with ±10% jitter.
- **Success**: 2xx only. 3xx is failure — redirects are never followed. 410 Gone disables the endpoint immediately.
- **Timeouts**: 15s total attempt budget, 5s connect.
- **Endpoint health**: an endpoint failing continuously for 5 days is auto-disabled (with anti-flap hysteresis) and surfaced to the tenant; re-enable is manual. Single-message redelivery is available in v0.1; bulk time-range replay is roadmap upside.

### Positive Consequences

- Matches the semantics receivers already implement for Svix/Stripe-class senders — including the ~24h+ outage-survival window.
- A fixed published ladder is testable and documentable; support questions have exact answers.

### Negative Consequences

- At-least-once means duplicates under retry races; the dedup burden is explicitly on receivers.
- A fixed ladder ignores endpoint-specific recovery patterns; no adaptive scheduling.

## Links

- Evidence: `docs/research/2026-07-25/rq01-delivery-semantics-and-retry-policy.md`
- Related: ADR-0005, ADR-0006
