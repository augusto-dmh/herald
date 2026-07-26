# ADR-0006: SSRF-safe egress at dial time and a bounded, polite dispatcher

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: security, http, concurrency

## Context and Problem Statement

Herald POSTs attacker-influenced URLs from inside its operator's network, at fan-out concurrency, against endpoints that may be slow, dead, or malicious — the outbound path must be safe and bounded by construction.

## Decision Drivers

- An outbound webhook sender is an SSRF machine; registration-time URL checks are defeated by DNS rebinding.
- No Transport-level timeout covers a server that returns headers then trickles the body.
- A slow endpoint must never occupy global delivery capacity.
- Reused connections require drained, closed response bodies — hygiene the hot path cannot skip.

## Considered Options

**Dial-time `Control`-func policy + bounded client** · **Registration-time URL validation only** · **Add a per-endpoint circuit breaker (gobreaker)**

## Decision Outcome

Chosen: **Dial-time `Control`-func policy + bounded client**, with:

- **SSRF**: a custom dialer whose `Control` func validates the actual dialed IP (post-DNS, pre-connect, on every dial): public unicast only (IPv6 `Unmap`-ed), ports 80/443 only. HTTPS-only outside a dev flag. Redirects never followed (`ErrUseLastResponse`), so no unvalidated hop exists.
- **Timeouts**: per-attempt context deadline enforcing ADR-0003's 15s budget; `Client.Timeout` slightly above it as backstop; 5s dial, 5s TLS handshake, 10s response-header at the Transport.
- **Pooling**: tuned Transport (`MaxIdleConnsPerHost` matching the per-endpoint cap, bounded `MaxIdleConns`/`MaxConnsPerHost`) sized for many distinct hosts.
- **Body hygiene**: capture the first 4 KiB of the response for the attempt log via `io.LimitReader`; drain up to 64 KiB then close — beyond that the connection is not worth saving.
- **Politeness**: a per-endpoint semaphore acquired with `TryAcquire`; on contention the job is snoozed (drover restores the attempt counter, so politeness never consumes retry budget). Global concurrency and per-endpoint caps are separate knobs.
- **No circuit breaker in v0.1**: endpoint disabling (ADR-0003) plus the durable ladder already fill that niche with persistent state; an in-memory breaker would duplicate it.

### Positive Consequences

- DNS rebinding, redirect laundering, and metadata-service access are closed at the only layer that sees the real IP.
- Every resource — time per attempt, connections, per-endpoint pressure — has an explicit bound.

### Negative Consequences

- Blocking non-standard ports and redirects rejects some legitimate-but-unusual receiver setups.
- Per-endpoint semaphores are in-process state; multi-instance deployments enforce politeness only per instance.

## Links

- Evidence: `docs/research/2026-07-25/rq04-outbound-http-reliability-in-go.md`, `docs/research/2026-07-25/rq02-webhook-security.md`
- Related: ADR-0003, ADR-0005
