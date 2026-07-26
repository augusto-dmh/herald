# ADR-0004: Standard Webhooks signing, signed-per-attempt, with 24h dual-signing rotation

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: security, signing

## Context and Problem Statement

Receivers must be able to verify that a delivery came from herald and is not a replay, and tenants must be able to rotate a compromised secret without dropping verifiable deliveries — all against a scheme receivers can implement without herald-specific tooling.

## Decision Drivers

- The Standard Webhooks specification consolidates Stripe/Svix practice and ships verification SDKs in nine languages — herald gets a receiver ecosystem for free.
- Replay protection requires the timestamp inside the signed content; a bare body HMAC (GitHub-style) has no replay defense.
- Retry backoff spans hours, so any per-message timestamp would age out of every sane tolerance window.
- Rotation must never force a hard cutover on receivers.

## Considered Options

**Standard Webhooks `v1` (symmetric HMAC)** · **Stripe-style custom header scheme** · **Standard Webhooks `v1a` (ed25519) from day one**

## Decision Outcome

Chosen: **Standard Webhooks `v1`**, with:

- **Scheme**: HMAC-SHA256 over `{msg_id}.{timestamp}.{raw_body}`, base64, `v1,`-prefixed, in the `webhook-id` / `webhook-timestamp` / `webhook-signature` headers.
- **Signed per attempt**: `webhook-id` is stable across retries (the receiver's dedup key); `webhook-timestamp` is fresh at each attempt, keeping every retry inside the documented ±5 minute verification tolerance.
- **Secrets**: per-endpoint, `whsec_`-prefixed, stored in an `endpoint_secrets` table; retrievable through an authenticated call (ecosystem practice, needed for receiver setup).
- **Rotation**: creating a new secret keeps the old one signing for 24h; deliveries carry multiple space-delimited signatures during the overlap, then the old secret expires.

### Positive Consequences

- Receivers verify with off-the-shelf standard-webhooks libraries; herald documents a tolerance, not a bespoke algorithm.
- Sign-per-attempt makes replay windows and retries compatible instead of contradictory.

### Negative Consequences

- Symmetric secrets mean the receiver can forge its own deliveries; parties needing third-party proof must wait for the deferred `v1a` ed25519 variant.
- The `endpoint_secrets` overlap table adds state the delivery path must join on.

## Links

- Evidence: `docs/research/2026-07-25/rq02-webhook-security.md`
- Related: ADR-0003, ADR-0006
