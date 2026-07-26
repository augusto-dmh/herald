# RQ04 — Outbound HTTP reliability in Go

Research date: 2026-07-25. Scope: mechanism-level analysis of `net/http` client timeouts, connection-pool behavior under diverse-host fan-out, body hygiene, per-endpoint politeness, circuit breakers, SSRF-safe egress, and how delivery attempts should map onto drover jobs — to ground herald v0.1's dispatcher design.

## 1. The timeout taxonomy: which layer catches which failure

An outbound HTTP attempt is a pipeline: DNS resolve → TCP connect → TLS handshake → write request → wait for response headers → read response body. Go exposes a timeout knob at almost every stage, plus two "whole attempt" mechanisms, and they fail differently.

### 1.1 Client.Timeout — the whole exchange, including the body

`http.Client.Timeout` "covers the entire exchange, from Dial (if a connection is not reused) to reading the body" (Cloudflare guide; confirmed by `net/http` docs). Mechanically it arms a timer when `Do` is called; when it fires, the request is cancelled *and* any in-progress `resp.Body.Read` returns an error. It also spans redirect chains. It is the only single-field way to bound a slow body read.

### 1.2 Transport-level timeouts — per-phase, body excluded

Each `http.Transport` field bounds one phase of one attempt:

| Field | Phase bounded | What it does NOT catch |
|---|---|---|
| `DialContext` (`net.Dialer.Timeout`) | DNS + TCP connect. Divided among candidate IPs when a host resolves to several (Happy Eyeballs, `FallbackDelay` default 300 ms) | anything after connect |
| `TLSHandshakeTimeout` | TLS negotiation | plaintext phases |
| `ResponseHeaderTimeout` | from end of request write to first response header bytes | request-body write time; response-body read time |
| `ExpectContinueTimeout` | wait for `100 Continue` when `Expect:` is set | everything else |
| `IdleConnTimeout` | how long an idle pooled connection is kept | not request-scoped at all — pool housekeeping |

Two documented holes: "there's no way to limit the time spent sending the request specifically" (the request-body write is only bounded by the overall deadline), and nothing at Transport level bounds the response-body read.

### 1.3 Context deadline — the per-attempt budget

`http.NewRequestWithContext(ctx, ...)` with `context.WithTimeout` cancels *every* phase including body reads, per attempt, under caller control. This is the idiomatic mechanism for a job-executing worker: the deadline composes naturally with drover's per-job child context.

### 1.4 The slow-body-trickle case

A malicious or broken receiver can accept the TCP connection fast, send `200 OK` plus headers immediately, then trickle the body at one byte per second. Dial, TLS, and `ResponseHeaderTimeout` all pass; no Transport timeout ever fires. Only `Client.Timeout` or the request context deadline caps this. Since herald reads response bodies (to capture a snippet for delivery logs, §3), every attempt must run under a context deadline, with `Client.Timeout` as a slightly larger safety net for code paths that forget the context:

```go
transport := &http.Transport{
    DialContext: (&net.Dialer{
        Timeout:   5 * time.Second,
        KeepAlive: 30 * time.Second,
        Control:   safeControl, // §6
    }).DialContext,
    TLSHandshakeTimeout:   5 * time.Second,
    ResponseHeaderTimeout: 10 * time.Second,
    // pool fields: §2
}
client := &http.Client{
    Transport: transport,
    Timeout:   35 * time.Second, // backstop only
    CheckRedirect: func(*http.Request, []*http.Request) error {
        return http.ErrUseLastResponse // §6.3
    },
}

// per attempt, inside the drover worker:
ctx, cancel := context.WithTimeout(jobCtx, 30*time.Second)
defer cancel()
req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(payload))
```

Budget arithmetic: phase timeouts must sum to less than the attempt deadline or they are dead code past the deadline; the attempt deadline must be less than drover's per-job timeout; the job timeout must be less than the lease duration (heartbeats extend leases, but the first lease must survive the first job).

## 2. Connection-pool tuning for fan-out to thousands of distinct hosts

### 2.1 The defaults and what they were designed for

`http.DefaultTransport` (Go 1.24 source): `MaxIdleConns: 100`, `IdleConnTimeout: 90s`, `TLSHandshakeTimeout: 10s`, `ExpectContinueTimeout: 1s`, dialer `Timeout: 30s, KeepAlive: 30s`, `ForceAttemptHTTP2: true`. `DefaultMaxIdleConnsPerHost = 2`. `MaxConnsPerHost` defaults to 0 = unlimited. These defaults assume a client talking to a handful of hosts.

### 2.2 Diverse hosts: keep-alive economics collapse gracefully

The idle pool is keyed per connection target (scheme + host + proxy identity). With thousands of distinct endpoint hosts and a global cap of `MaxIdleConns: 100`, most hosts will never find an idle connection: every delivery to a long-tail host pays DNS + TCP + TLS anew. That is fine — the marginal cost is tens to low hundreds of milliseconds per attempt, dwarfed by webhook retry timescales. The tuning goal for the long tail is not hit rate but *bounded resource usage*: a lower `IdleConnTimeout` (30s) releases file descriptors for hosts that will not be revisited soon, and `MaxIdleConns` caps total idle FDs regardless of host count.

### 2.3 Concentrated hosts: the `MaxIdleConnsPerHost=2` churn trap

The classic failure mode is the opposite distribution: a few popular endpoints receive most deliveries. If 10 workers concurrently deliver to the same host with the default per-host idle cap of 2, then 8 of the 10 finished connections *cannot* be parked — they are closed, and the next 8 requests dial fresh. Symptoms: handshake CPU, elevated latency, and `TIME_WAIT` socket accumulation on the sender. Fix: raise `MaxIdleConnsPerHost` to at least the per-endpoint concurrency cap (§4), e.g. 8–16.

### 2.4 `MaxConnsPerHost` as a backstop, not a scheduler

`MaxConnsPerHost` caps dialing + active + idle connections per host; when the cap is hit, requests *block inside the Transport* waiting for a connection. That makes it a crude per-host concurrency limiter — but the queueing is invisible (no metric, no context for fairness), and a worker blocked inside `RoundTrip` still occupies a global pool slot. Use an explicit semaphore for policy (§4) and set `MaxConnsPerHost` (e.g. 16) only as a defense-in-depth cap against limiter bugs.

### 2.5 DNS behavior

Go's stdlib resolver performs a lookup per dial and does not cache results client-side. On Linux the pure-Go resolver reads `/etc/resolv.conf` and queries directly (a blocked lookup costs a goroutine, not a thread); the cgo path is used only when the config demands it, capped at 500 concurrent lookups. For a fan-out sender this means: per-attempt DNS latency is on the critical path for every non-reused connection; an OS-level cache (systemd-resolved, nscd) or a local caching resolver is the right mitigation, not an in-process cache — and *no* in-process DNS pinning should be added, because the SSRF `Control` check (§6) depends on validating the address actually dialed each time.

## 3. Response body hygiene

### 3.1 Read to EOF and close, or lose the connection

The `net/http` documentation is explicit: "If the Body is not both read to EOF and closed, the Client's underlying RoundTripper (typically Transport) may not be able to re-use a persistent TCP connection." Mechanism: unread bytes are still in flight on the TCP stream; the Transport cannot return the connection to the pool without knowing the response is fully consumed, so `Close` on a partially-read body tears down the TCP connection.

### 3.2 Drain up to a cap; a huge body is not worth a connection

Draining is only economical up to a point — reading a 50 MB error page to save one handshake is a bad trade. The standard pattern: capture what you need, drain a bounded amount more, then close; if the body exceeds the drain cap, accept the connection loss.

```go
const captureCap = 4 << 10 // stored in delivery log
const drainCap   = 64 << 10 // beyond this, closing the conn is cheaper

snippet, err := io.ReadAll(io.LimitReader(resp.Body, captureCap))
_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, drainCap))
resp.Body.Close()
```

`io.LimitReader` also caps memory: an endpoint cannot make herald buffer an arbitrarily large response into its delivery log. (`http.MaxBytesReader` is the server-side sibling; for client responses `LimitReader` is the tool.) The same discipline is correct under HTTP/2 — failing to drain wastes a stream rather than a TCP connection, but the code is identical.

## 4. Per-endpoint concurrency control and rate limiting

### 4.1 Two separate knobs

Global worker-pool size protects **herald** (memory, FDs, DB load, total in-flight work). Per-endpoint politeness protects **receivers** (and other tenants' latency). They are not interchangeable: a 100-worker pool with no per-endpoint limit will happily point all 100 workers at one hot endpoint — hammering it, and starving every other tenant's deliveries while those workers wait out 30-second timeouts.

### 4.2 Token bucket: `golang.org/x/time/rate`

`rate.NewLimiter(r, b)` is a token bucket of capacity `b` refilled at `r` tokens/second. `Allow()` returns false when empty (drop/defer); `Reserve()` returns how long to wait without blocking; `Wait(ctx)` blocks, failing fast if the expected wait exceeds the context deadline. Limiters are goroutine-safe and cheap (a few words of state).

### 4.3 Semaphore per endpoint

`semaphore.Weighted` (or a buffered channel) caps *concurrent in-flight* requests per endpoint. Rate and concurrency bind in different regimes: against a slow endpoint the semaphore binds (10 in-flight × 30 s each = 0.33 rps regardless of limiter); against a fast endpoint the rate limiter binds. Hookdeck's guidance for senders is on the order of 10 concurrent per endpoint.

### 4.4 Per-key maps and eviction

Neither package provides a keyed collection; the pattern is a mutex-guarded `map[endpointID]*limiterState`. Generic per-key limiter maps need eviction (last-seen sweep or LRU) because keys are unbounded. Herald's keys are *registered endpoints* — bounded, DB-backed rows — so a plain map evicted on endpoint delete/disable suffices; no LRU machinery is warranted.

### 4.5 Never block a global worker on a per-endpoint limit

The critical interaction with drover: `sem.Acquire(ctx)`/`limiter.Wait(ctx)` inside a worker parks a *global* pool slot on one endpoint's queue — head-of-line blocking that converts one slow endpoint into fleet-wide throughput loss. The non-blocking form returns the slot to the pool:

```go
if !state.sem.TryAcquire(1) || !state.limiter.Allow() {
    return drover.Snooze(2 * time.Second) // job re-runs later; worker slot freed
}
defer state.sem.Release(1)
```

The same shape handles `429 Too Many Requests`: parse `Retry-After`, `Snooze` for that duration, and optionally `SetLimit` down on the endpoint's limiter.

## 5. Circuit breakers: mechanics, and whether herald needs one

### 5.1 sony/gobreaker

Three states. **Closed**: requests flow; `ReadyToTrip(counts)` (default: >5 consecutive failures) trips to open. **Open**: `Execute` returns `ErrOpenState` without calling the function; after `Timeout` (default 60 s) → half-open. **Half-open**: up to `MaxRequests` probes (default 1); excess probes get `ErrTooManyRequests`; a probe success closes (resetting counts), a failure re-opens. `IsSuccessful` classifies errors; v2 adds `IsExcluded` (e.g. ignore context cancellations) and rolling-window counting via `BucketPeriod`. `TwoStepCircuitBreaker.Allow() → done(err)` decouples the permission check from execution — the right shape for async job workers.

### 5.2 failsafe-go

Richer thresholding: count-based (`WithFailureThresholdRatio(3, 5)`) and time-based (`WithFailureRateThreshold(0.2, 5, time.Minute)`) windows, configurable open→half-open delay, and policy composition (retry ∘ breaker ∘ timeout). Its docs carry one important composition rule: a breaker must not count rejections from co-located rate limiters/bulkheads as failures — those are self-inflicted, not evidence the dependency is down.

### 5.3 Honest analysis: mostly redundant in herald v0.1

Herald v0.1 already ships two mechanisms occupying the breaker's niche:

1. **Per-message retry backoff** (drover, `attempt^4` ± 10 % jitter): a failing delivery does not hammer the endpoint — its retries space out to minutes, then hours, automatically.
2. **Endpoint auto-disabling** on sustained failure: this *is* a circuit breaker at endpoint granularity — trip condition = failure threshold, open = disabled. What it lacks is the half-open probe (automatic recovery); v0.1 recovers by explicit re-enable.

What a dedicated per-endpoint breaker would add is the window *between* first failures and the disable threshold: fresh messages to a dying endpoint still burn full timeout budgets (30 s of a worker slot each) until disabling kicks in. That is a real cost, but it is bounded by the disable threshold and per-endpoint concurrency cap (at most `sem_cap` workers can be stuck on one endpoint), and the delivery-log table already contains the consecutive-failure state a breaker would maintain in memory — an in-memory breaker per endpoint would be a *second*, unsynchronized copy of that state, which drifts across dispatcher restarts and (later) multiple dispatcher nodes, since gobreaker/failsafe-go state is process-local. Verdict for v0.1: no breaker library; treat "disable policy + eventual automatic re-enable probe" as the breaker, implemented against durable state. Revisit if p99 worker-slot occupancy on dead endpoints shows up in metrics.

## 6. SSRF-safe egress

### 6.1 Why validating the URL is not enough

Herald dials attacker-supplied URLs by design. Checking the hostname's resolved IP *before* the request has two documented bypasses (agwa.name): **DNS rebinding / TOCTOU** — a resolver that answers a public IP at validation time and `169.254.169.254` at dial time — and redirects to internal hosts. The check must happen at the socket, on the address actually being dialed.

### 6.2 `net.Dialer.Control`

`Control func(network, address string, c syscall.RawConn) error` runs after the socket is created but *before* connecting, and `address` is always the literal resolved `ip:port`. Rejecting there closes the TOCTOU gap for every connection the Transport makes:

```go
func safeControl(network, address string, _ syscall.RawConn) error {
    host, port, err := net.SplitHostPort(address)
    if err != nil {
        return err
    }
    if port != "80" && port != "443" {
        return fmt.Errorf("egress: port %s not allowed", port)
    }
    ip, err := netip.ParseAddr(host)
    if err != nil {
        return err
    }
    ip = ip.Unmap()
    if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
        ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
        return fmt.Errorf("egress: %s is not a public address", ip)
    }
    return nil
}
```

(`IsLinkLocalUnicast` covers `169.254.0.0/16`, i.e. cloud metadata endpoints; production should use a full deny-list of special-purpose ranges — `code.dny.dev/ssrf` maintains one, including IPv6 ULA/NAT64/documentation ranges.) Because the check lives in the dialer, it covers every protocol and every connection, not just the first request.

### 6.3 Redirects: covered, but disable them anyway

Redirect hops are issued through the same Transport, hence the same dialer and the same `Control` check — re-validation of every hop is automatic with this design (unlike URL-level validation, which checks only hop zero). Herald should still disable redirect-following (`CheckRedirect` returning `http.ErrUseLastResponse`) and record the 3xx as the delivery outcome: a webhook receiver redirecting a signed POST is at best misconfigured, redirect-following can silently re-send the signed body to a different host, and the 3xx status is more useful in a delivery log than the terminal hop's result. This is also what makes the security argument auditable: no hop chasing to reason about at all.

## 7. Worker topology on drover

### 7.1 Drover's actual model (from README + ADR-0003)

Facts that constrain the design: at-least-once via committed claim with `leased_until` lease + heartbeat + rescuer sweep; retries at `attempt^4` seconds with ±10 % jitter, default max 25 attempts, **pluggable `RetryPolicy`**; `Cancel` and `Snooze` sentinel errors classify non-retryable and deferred outcomes; exhausted jobs land in a retained `dead` state with scoped redrive; executor is a fixed goroutine pool fed over channels, per-job child context with timeout, panic recovery at the job boundary; transactional enqueue (`InsertTx`).

### 7.2 Granularity: job-per-attempt vs job-per-delivery

**Job per attempt** (each attempt is a fresh job; on failure the worker inserts a new job with `scheduled_at` in the future): herald re-implements attempt counting, backoff computation, jitter, and terminal-state ("dead") bookkeeping that drover already owns — and the insert-next-attempt step is a new failure mode (crash between "attempt failed" and "next attempt inserted" strands the delivery, unless done transactionally with the outcome write, which is exactly the machinery drover's retry path already is).

**Job per delivery** (one drover job per (message, endpoint) pair, living across attempts): a failed attempt returns a retryable error and drover schedules the retry (`attempt^4` + jitter); a non-retryable outcome (4xx other than 408/425/429) returns `Cancel`; a 429 or a per-endpoint politeness rejection returns `Snooze`; attempt exhaustion lands in `dead` — which is precisely herald's "delivery failed permanently" state, with redrive as manual re-delivery. Drover's attempt counter *is* the delivery attempt counter; the delivery-log row per attempt is written by the worker before returning.

Fan-out happens at enqueue: the API handler inserts one job per subscribed endpoint in the same transaction as the message row (`InsertTx`) — per-endpoint failure isolation then costs nothing, and a disabled endpoint simply has its pending jobs cancelled or snoozed.

### 7.3 Backoff shape and attempt budget

`attempt^4` seconds gives 1 s, 16 s, 81 s, ~4 min, ~10 min, ~22 min, ~40 min, ~68 min…; 10 attempts span roughly 7 hours cumulative. Webhook providers commonly retry for 1–3 days with coarser tiers (Hookdeck: 1 min → 5 min → 15 min → 1 h → 4 h, then 6/12/24 h out to 3–7 days). Drover's default curve with max-attempts ≈ 10 is a defensible v0.1 (a receiver down for 7 h straight will likely trip endpoint disabling anyway); a multi-day tail is a custom `RetryPolicy`, not new machinery.

### 7.4 Pool sizing and backpressure

Delivery workers are almost pure I/O wait: worker count N ≈ desired concurrent deliveries, and per-goroutine cost is kilobytes of stack plus one in-flight request's buffers — 64–128 workers on one node is unremarkable. The DB cost of N is bounded by drover's batch fetch/ack, not one connection per worker. Backpressure is inherent to the pull model: if enqueue outpaces delivery, depth grows *in Postgres* (durable, observable via oldest-job age — drover's primary alerting metric) rather than in process memory; the dispatcher never needs to shed load, though the ingest API may throttle producers on queue depth if desired. The one rule that keeps the pool live is §4.5: per-endpoint waits become `Snooze`, never in-worker blocking, so a slow or dead endpoint can occupy at most its semaphore cap of the pool.

## Comparison table

Mechanisms for protecting receivers and the pool from a misbehaving endpoint:

| Mechanism | Bounds | State location | Blocks a worker slot? | Auto-recovery | Marginal cost in herald v0.1 | Verdict for v0.1 |
|---|---|---|---|---|---|---|
| Per-attempt ctx deadline (§1) | wall-time per attempt | none | yes, up to deadline | n/a | required anyway | **in** |
| Per-endpoint semaphore (§4.3) | concurrent in-flight per endpoint | in-memory map (bounded keys) | no (TryAcquire + Snooze) | immediate | small map + sentinel returns | **in** |
| Per-endpoint `rate.Limiter` (§4.2) | request rate per endpoint | in-memory map | no (Allow + Snooze) | immediate | small; needs a rate policy per endpoint | optional (defer unless 429s observed) |
| `MaxConnsPerHost` (§2.4) | conns per host, Transport-wide | Transport | yes, invisibly | immediate | one field | backstop only |
| Retry backoff (drover, §7.3) | per-message pressure over time | Postgres (job row) | no | built-in | free — drover owns it | **in** |
| Endpoint disabling policy | all traffic to a failing endpoint | Postgres (endpoint row + logs) | no | manual (v0.1); probe job later | core product feature | **in** |
| gobreaker / failsafe-go per endpoint (§5) | fresh sends to a failing endpoint pre-disable | process memory (unsynchronized, lost on restart) | no | half-open probe | duplicate of durable failure state | **out** for v0.1 |

## Gap analysis

- **Endpoint-host distribution is unknown.** §2's tuning depends on whether real deployments are long-tail-diverse or concentrated on a few hosts; the recommended values hedge both but should be revisited with production metrics (idle-pool hit rate, handshake counts).
- **Drover `Snooze` semantics are underspecified in its docs.** Neither the README nor ADR-0003 states whether a snoozed job consumes an attempt or how snooze interacts with `max attempts`; herald's 429/politeness design (§4.5, §7.2) assumes snooze does *not* burn attempts. Must be confirmed against drover's implementation (drover is pre-v0.1; its `RetryPolicy` surface may still shift).
- **Automatic endpoint re-enable (half-open probe) is deferred.** v0.1 disables endpoints without auto-recovery; the natural follow-up is a periodic probe job that re-enables on success — effectively completing the durable breaker of §5.3.
- **HTTP/2 to arbitrary endpoints is untested.** `ForceAttemptHTTP2: true` is the default; misbehaving h2 receivers are a possible source of odd failure modes, and falling back to an HTTP/1.1-only transport is a one-line mitigation if they appear.
- **DNS latency on the attempt critical path** (§2.5) is accepted, not measured; if it matters, the fix is infrastructure (local caching resolver), not code.
- **Multi-node dispatchers** are out of scope here: per-endpoint semaphores/limiters are per-process, so cross-node per-endpoint caps would need a shared mechanism (or acceptance that the cap is per-node). Fine at v0.1's single-node scale; must be restated if herald scales out.

## Options

**Option 1 — Stock client: `http.DefaultClient` + `Client.Timeout` only.** Why: zero code; `Client.Timeout` does cap the slow-body case. Why not: no SSRF `Control` hook (disqualifying for a webhook sender on its own), redirects followed by default, `MaxIdleConnsPerHost=2` churn on hot endpoints, no per-phase timeouts so every failure mode costs the full budget, and 30 s dialer timeout wastes worker time on dead hosts.

**Option 2 — One shared tuned Transport + per-attempt context deadline. ★ RECOMMENDED.** Concrete values: dialer `Timeout: 5s`, `KeepAlive: 30s`, `Control: safeControl` (deny non-public IPs and non-80/443 ports, per §6); `TLSHandshakeTimeout: 5s`; `ResponseHeaderTimeout: 10s`; `IdleConnTimeout: 30s`; `MaxIdleConns: 256`; `MaxIdleConnsPerHost: 8`; `MaxConnsPerHost: 16` (backstop); redirects disabled via `http.ErrUseLastResponse`, 3xx recorded as the outcome; per-attempt budget `context.WithTimeout(jobCtx, 30s)` with `Client.Timeout: 35s` as a backstop; response capture 4 KiB via `io.LimitReader`, drain up to 64 KiB then close; drover job timeout 40 s < lease duration. Why: every failure mode in §1 is caught by the cheapest layer that can catch it, the slow-body case is bounded twice, one Transport keeps pool metrics and FD budgets global, and SSRF checks sit at the socket where TOCTOU cannot bypass them. Why not: per-phase values are judgment calls pending production data, and a single Transport means one endpoint's TLS quirks can't get bespoke settings (acceptable: v0.1 has no per-endpoint TLS config).

**Option 3 — `http.Client` per endpoint.** Why: per-endpoint timeout/TLS customization; `MaxConnsPerHost` becomes a true per-endpoint cap. Why not: thousands of Transports fragment the FD budget and idle pools, defeat global `MaxIdleConns`, and duplicate what a semaphore map does in a few lines. Rejected for v0.1.

**Per-endpoint politeness — blocking waits (`sem.Acquire`/`limiter.Wait`) in workers.** Why: simplest code; context-aware. Why not: parks global worker slots on one endpoint's queue — head-of-line blocking (§4.5). Rejected.

**Per-endpoint politeness — `TryAcquire`/`Allow` + `Snooze`, semaphore cap 8 per endpoint. ★ RECOMMENDED.** Why: converts per-endpoint contention into rescheduling, keeps the pool live, matches drover's sentinel model, and the cap doubles as the sizing input for `MaxIdleConnsPerHost`. Rate limiters deferred until 429 pressure is observed (then: honor `Retry-After` via `Snooze` first, limiter second). Why not: snooze-retry adds queue churn under sustained contention — bounded and observable, and cheaper than a starved pool.

**Circuit breaker — gobreaker (or failsafe-go) per endpoint.** Why: fast-fails fresh sends to a dying endpoint before the disable threshold; half-open gives auto-recovery. Why not (v0.1): duplicates durable failure state (delivery logs + disable policy) in unsynchronized process memory, resets on restart, and the cost it saves is already capped by the per-endpoint semaphore. Out for v0.1.

**Circuit breaker — endpoint-disabling policy as the (durable) breaker. ★ RECOMMENDED.** Trip on consecutive-failure threshold from delivery logs; open = disabled; add a probe/re-enable job post-v0.1 to complete the half-open leg. Why: one source of truth, survives restarts, multi-node-safe by construction. Why not: coarser reaction time than an in-memory breaker — accepted trade.

**Drover granularity — one job per delivery attempt.** Why: jobs are short and uniform. Why not: re-implements retry scheduling, attempt counting, jitter, and dead-lettering that drover owns, and adds a crash window around next-attempt insertion. Rejected.

**Drover granularity — one job per (message, endpoint), attempts = drover retries. ★ RECOMMENDED.** Fan-out at enqueue via `InsertTx` (one job per subscribed endpoint, same transaction as the message). Retryable failures return an error (drover schedules `attempt^4` ± 10 % jitter); non-retryable 4xx → `Cancel`; 429/politeness → `Snooze`; exhaustion → `dead` = permanent failure with redrive as manual re-delivery. Max attempts ≈ 10 (~7 h span) for v0.1; multi-day tails via a custom `RetryPolicy` later. Pool: 64–128 workers, sized by target concurrent deliveries. Why: maximal reuse of audited drover semantics, transactional fan-out for free, per-endpoint isolation by construction. Why not: ties herald's retry curve to drover's policy surface (pre-v0.1, may shift — tracked in Gap analysis), and per-attempt observability must come from herald's delivery-log writes rather than one-row-per-attempt jobs.

## Sources

- https://blog.cloudflare.com/the-complete-guide-to-golang-net-http-timeouts/ (accessed 2026-07-25) — canonical taxonomy of client/transport timeouts; source of the "no way to limit request-send time" and Client.Timeout-covers-body claims.
- https://pkg.go.dev/net/http#Transport (accessed 2026-07-25) — Transport field semantics, `DefaultMaxIdleConnsPerHost = 2`, and the read-to-EOF-and-close connection-reuse requirement.
- https://raw.githubusercontent.com/golang/go/go1.24.0/src/net/http/transport.go (accessed 2026-07-25) — exact `DefaultTransport` literal: MaxIdleConns 100, IdleConnTimeout 90s, TLSHandshakeTimeout 10s, ExpectContinueTimeout 1s, dialer 30s/30s, ForceAttemptHTTP2 true.
- https://pkg.go.dev/net#Dialer (accessed 2026-07-25) — Dialer.Timeout division across IPs, FallbackDelay 300 ms (RFC 6555), `Control`/`ControlContext` call point and resolved-address argument, pure-Go vs cgo resolver behavior.
- https://pkg.go.dev/golang.org/x/time/rate (accessed 2026-07-25) — token-bucket mechanics: Allow/Reserve/Wait, burst semantics, Wait vs context deadline, SetLimit/SetBurst.
- https://pkg.go.dev/golang.org/x/sync/semaphore (accessed 2026-07-25) — Weighted semaphore Acquire/TryAcquire/Release mechanics.
- https://pkg.go.dev/github.com/sony/gobreaker/v2 (accessed 2026-07-25) — Settings, state machine, default ReadyToTrip (>5 consecutive failures), ErrOpenState/ErrTooManyRequests, TwoStepCircuitBreaker.
- https://failsafe-go.dev/circuit-breaker/ (accessed 2026-07-25) — count- vs time-based thresholds, ratio/rate thresholds, half-open delay, rule against counting limiter rejections as breaker failures.
- https://www.agwa.name/blog/post/preventing_server_side_request_forgery_in_golang (accessed 2026-07-25) — DNS-rebinding/TOCTOU argument and the `Control`-hook validation pattern on the resolved address.
- https://pkg.go.dev/code.dny.dev/ssrf (accessed 2026-07-25) — maintained deny-list of special-purpose IPv4/IPv6 ranges packaged as a `Control` func.
- https://blog.doyensec.com/2022/12/13/safeurl.html (accessed 2026-07-25) — safeurl-for-Go design notes: Control-hook IP validation and CheckRedirect handling.
- https://hookdeck.com/blog/building-reliable-outbound-webhooks (accessed 2026-07-25) — practitioner numbers: 5 s connect / 30 s request timeouts, tiered multi-day retry schedule, ~10 concurrent per endpoint, 429/Retry-After handling, per-endpoint breaker triggers.
- /home/augusto/projects/drover/README.md and /home/augusto/projects/drover/docs/adr/0003-at-least-once-delivery-lease-heartbeat-rescuer.md (local, read 2026-07-25) — drover's lease/heartbeat/rescuer model, `attempt^4` ± 10 % retry curve, max attempts 25 default, `Cancel`/`Snooze` sentinels, dead-state redrive, fixed-pool executor, transactional enqueue.

**Unverified claims flagged:** the idle-connection pool being keyed per (scheme, host, proxy) and its eviction discipline under the global `MaxIdleConns` cap are from prior reading of `net/http/transport.go` internals, not re-verified today; the claim that redirect hops necessarily re-enter the same dialer `Control` hook is mechanically implied by the Transport architecture but was not confirmed by a primary source in this session (herald disables redirects, making it moot); drover `Snooze`'s interaction with the attempt counter is unspecified in the documents read and is assumed non-consuming (tracked in Gap analysis); Hookdeck's timeout/concurrency figures are one vendor's recommendations, not measurements; the "tens to low hundreds of milliseconds" cost of a cold DNS+TCP+TLS attempt and the "kilobytes of stack per goroutine" sizing figure are order-of-magnitude estimates from general experience, not benchmarks run for this document.
