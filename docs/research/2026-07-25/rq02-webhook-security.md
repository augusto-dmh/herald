# RQ02 — Webhook security: signing, replay protection, and outbound-request safety

Research date: 2026-07-25. Scope: mechanism-level analysis of webhook payload signing (Standard Webhooks, Stripe, GitHub), replay protection, secret lifecycle management, and outbound-request (SSRF) hardening, to ground herald v0.1's delivery-security design.

## 1. The Standard Webhooks specification

Standard Webhooks (standard-webhooks.org, spec maintained on GitHub) is a community specification with a steering committee drawn from Zapier, Twilio, Svix, Kong, and Supabase, with adoption by OpenAI, Anthropic, and others. Its stated goal: "the ecosystem is fragmented, with each webhook provider using different implementations and varying quality" — the spec aims to do "for webhooks what JWT did for API authentication." It is essentially a formalization of Svix's scheme (Svix uses `svix-id`/`svix-timestamp`/`svix-signature` header aliases with identical semantics).

### 1.1 Headers

Every signed delivery carries three headers:

```
webhook-id: msg_2KWPBgLlAfxdpx2AI54pPJ85f4W
webhook-timestamp: 1674087231
webhook-signature: v1,K5oZfzN95Z9UVu1EsfQmfVNQhnkZ2pj9o9NDN/H/pI4=
```

- `webhook-id` — unique message identifier, stable across retries of the same message (doubles as the consumer's idempotency key).
- `webhook-timestamp` — integer Unix timestamp in seconds, the time of *this delivery attempt*.
- `webhook-signature` — space-delimited list of signatures, each prefixed with a version identifier and a comma.

### 1.2 Signed content construction

The signed content is the message ID, timestamp, and raw body "concatenated (delimited by full-stops)":

```
signed_content = "{webhook-id}.{webhook-timestamp}.{raw_body}"
```

e.g. `msg_2KWPBgLlAfxdpx2AI54pPJ85f4W.1674087231.{"type":"contact.created",...}`.

Two mechanism-level consequences:

1. **The bytes signed must be the bytes sent.** The spec is explicit that "the payload sent is the same as the payload signed" — any JSON re-serialization (key reordering, whitespace changes) on either side breaks verification. For herald this means: store the payload as the exact byte sequence received from the sender (Postgres `bytea` or verbatim `jsonb->text` captured once) and write those same bytes to the HTTP body.
2. **Including `webhook-id` binds the signature to a specific message** (a valid signature for message A can never be replayed as message B), and **including the timestamp enables replay expiry** (section 3).

### 1.3 Symmetric scheme (`v1`): HMAC-SHA256, base64

- MAC: HMAC-SHA256 over `signed_content`, keyed with the *decoded* secret.
- Encoding: standard base64 of the raw 32-byte MAC (not hex — this differs from Stripe/GitHub).
- Header form: `v1,<base64 mac>`.
- Secret format: base64, prefixed `whsec_`; the spec requires entropy "between 24 bytes (192 bits) and 64 bytes (512 bits)". The portion after `whsec_` is base64-decoded before use as the HMAC key — a classic implementation bug is HMACing with the ASCII secret string instead of the decoded bytes.
- Verification must use a constant-time comparison (`hmac.Equal` in Go).

Go sending-side sketch:

```go
mac := hmac.New(sha256.New, decodedSecret) // decodedSecret = base64.StdEncoding.Decode(strings.TrimPrefix(s, "whsec_"))
fmt.Fprintf(mac, "%s.%d.", msgID, timestamp)
mac.Write(rawBody)
sig := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
```

### 1.4 Asymmetric scheme (`v1a`): ed25519

For consumers who must verify without holding a shared secret (e.g. verification at the edge, or secrets held by a third party), the spec defines an ed25519 variant:

- Same `signed_content` construction; signature is ed25519 over it.
- Header form: `v1a,<base64 signature>`.
- Key formats: private key `whsk_<base64>`, public key `whpk_<base64>`.
- No constant-time concern on verify (signature verification is not secret-dependent), but key distribution becomes the hard problem.

The spec recommends symmetric keys be "unique per endpoint," and asymmetric keys "unique per endpoint (or potentially customer)."

### 1.5 Versioning

The `v1,`/`v1a,` prefix is the versioning mechanism: a future `v2` scheme can be shipped by adding a second space-delimited signature to the same header, letting consumers migrate without a flag day. Consumers are told to iterate signatures and "try to verify each signature until one matches," ignoring prefixes they do not understand.

## 2. Provider schemes as contrast: Stripe and GitHub

### 2.1 Stripe: `Stripe-Signature` with `t=` and `v1=`

```
Stripe-Signature: t=1492774577,v1=5257a869e7ecebeda32affa62cdca3fa51cad7e77a0e56ff536d0ce8e108d8bd,v0=6ffb...
```

- `signed_payload = "{t-value}.{raw_body}"` — timestamp, a literal `.`, then the raw request body.
- HMAC-SHA256, **hex** encoded, keyed with the endpoint secret; `v1` is the live scheme, `v0` is a legacy test-mode scheme to be ignored.
- The timestamp lives *inside the same header* as the signature rather than in its own header, and there is no message-ID inside the signed content (idempotency comes from the event `id` in the JSON body instead).
- Stripe's libraries enforce "a default tolerance of five minutes between the timestamp and the current time" and recommend NTP-synced clocks.
- Multiple `v1=` entries appear while rolling a secret: Stripe keeps the previous secret active "for up to 24 hours" after a roll and "generates one signature per secret until expiration." Secrets are retrievable in the dashboard ("Click to reveal").

### 2.2 GitHub: `X-Hub-Signature-256`

```
X-Hub-Signature-256: sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17
```

- HMAC-SHA256 **hex** digest of the raw body only, prefixed `sha256=`; secret is a user-chosen string (no format, no entropy floor).
- **No timestamp, no message ID in the signed content, and no documented replay protection at all** — a captured delivery verifies forever. Consumers are told to use constant-time compare (`secure_compare` / `crypto.timingSafeEqual`) and treat payloads as UTF-8, but replay defense is entirely absent.
- One secret per webhook; rotation is replace-and-pray (no dual-signing window).

### 2.3 Why Standard Webhooks consolidates these

The three schemes solve overlapping problems with incompatible envelope details: header name, encoding (base64 vs hex), what is signed (id+ts+body vs ts+body vs body), where the timestamp lives, and whether rotation is possible without an outage. Every consumer re-learns these per provider, and weaker designs (GitHub's) omit replay protection entirely. Standard Webhooks takes the strongest union — Stripe-style timestamped signing plus a message ID in the signed content, base64 encoding, typed secret format, versioned multi-signature header — and fixes the names so one verification library works everywhere.

## 3. Replay protection

### 3.1 Why the timestamp must be inside the signed content

A replay attack is an attacker (or a compromised log/proxy) re-transmitting a captured request, valid signature included. If the timestamp were only a plain header, the attacker would simply update it; because it is part of `signed_content`, any modification invalidates the MAC. The receiver's check is therefore: (a) verify the signature, (b) parse `webhook-timestamp`, (c) reject if `|now - ts| > tolerance`. Both checks are required — the timestamp bound is meaningless without the signature binding, and vice versa.

### 3.2 Tolerance windows in practice

- **Stripe:** 5 minutes default in official libraries.
- **Svix:** rejects "webhooks with a timestamp that are more than five minutes away (past or future) from the current time" — note the window is two-sided, tolerating consumer clock skew in either direction.
- **Standard Webhooks spec:** requires timestamp verification "within some allowable tolerance" without fixing a number; 5 minutes is the de-facto ecosystem value.

Both Stripe and Svix explicitly recommend NTP-synchronized clocks on the receiving side.

### 3.3 Sender-side consequence: sign per attempt, not per message

Because receivers enforce a ±5-minute window on `webhook-timestamp`, a sender that signs once at enqueue time will fail verification on any retry after the window (herald's drover-backed retries will routinely exceed 5 minutes with exponential backoff). Therefore **the signature must be computed at delivery-attempt time** with a fresh timestamp, while `webhook-id` stays constant across attempts so consumers can deduplicate. This is why `webhook-id` (per message) and `webhook-timestamp` (per attempt) are separate fields.

### 3.4 Idempotency

The spec designates `webhook-id` as the dedup key: at-least-once delivery plus retries means consumers will see duplicates, and replay-window enforcement alone does not stop a same-window duplicate. Herald should document that consumers key on `webhook-id`; herald itself guarantees the ID is stable across retries.

## 4. Secret management

### 4.1 Per-endpoint secrets and format

One secret per endpoint (Standard Webhooks: "signing keys should be unique per endpoint") — a leaked secret then compromises exactly one endpoint, and per-tenant isolation falls out for free in a multi-tenant system. Format `whsec_` + base64, with 24–64 bytes of entropy; herald should generate 32 bytes from `crypto/rand` (`whsec_` + 44 base64 chars). The typed prefix makes secrets greppable in leaked logs/repos (same rationale as `sk_live_` API keys) and self-describing for verification libraries.

### 4.2 Rotation with overlapping dual signatures

The zero-downtime rotation mechanism (Svix, Stripe both implement it; Standard Webhooks' multi-signature header exists for it):

1. Rotate creates a new secret; the old secret enters an expiry window (Svix: "the previous secret will remain valid for the next 24 hours"; Stripe: immediate or delayed "up to 24 hours").
2. During the window, every delivery is signed with **all** active secrets, producing a space-delimited header: `webhook-signature: v1,<sig-new> v1,<sig-old>`.
3. Consumers "try and match each signature and as long as one of them matches, consider it a success" — so a consumer holding either the old or new secret keeps verifying throughout.
4. After expiry, only the new secret signs.

Storage model for herald: an `endpoint_secrets` table (endpoint_id, secret, created_at, expires_at NULL = current), signer loads all non-expired secrets per endpoint. Rotation is an INSERT of the new secret plus an UPDATE stamping `expires_at = now() + interval '24 hours'` on the old one.

### 4.3 Secret exposure in APIs

Ecosystem practice is **retrievable, not write-only**: Stripe reveals endpoint secrets in the dashboard; Svix's API exposes a get-endpoint-secret operation alongside rotate. Rationale: a webhook signing secret is a shared symmetric key — the consumer *must* obtain it, and re-provisioning flows (new server, IaC, secret manager sync) need to re-read it; making it write-only only forces rotations without a security gain, since anyone with API access could rotate-and-read anyway. The compensating controls are: secrets retrievable only via authenticated tenant-scoped API, never included in list-endpoints responses by default (dedicated `GET .../secret` call), and excluded from logs.

## 5. SSRF: the outbound sender as a confused deputy

### 5.1 Why a webhook sender is an SSRF machine

Herald's core feature is "make my server issue an HTTP POST to an arbitrary URL supplied by a tenant." That is the textbook SSRF primitive, but productized: an attacker who can register an endpoint can aim herald's network position at `http://169.254.169.254/latest/meta-data/` (cloud metadata → credentials), `http://10.0.0.5:5432/`, `http://localhost:9090/` (internal admin planes), etc. Delivery logs make it worse: herald records response status/body, so it is a *read* SSRF, not just blind. Svix's own security docs name this the primary sender-side risk: "The main way to protect against SSRF is to prevent the webhooks from calling into internal networks and services" (they use the Smokescreen proxy plus network isolation of delivery workers).

### 5.2 What to block

OWASP's SSRF cheat sheet minimum deny set, extended by the IANA special-purpose registries (as codified in `code.dny.dev/ssrf`, which tracks them):

- IPv4: `0.0.0.0/8`, `127.0.0.0/8` (loopback), `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` (RFC1918), `169.254.0.0/16` (link-local — includes AWS/GCP/Azure metadata `169.254.169.254`), `100.64.0.0/10` (CGNAT), `192.0.0.0/24`, `192.0.2.0/24`, `198.51.100.0/24`, `203.0.113.0/24` (documentation), `198.18.0.0/15` (benchmarking), `224.0.0.0/4` (multicast), `240.0.0.0/4` (reserved).
- IPv6: allow only global unicast `2000::/3`, and within it still deny `2001:db8::/32` (documentation), `2002::/16` (6to4), `2001::/23` (special-purpose); deny `::1/128`, `fc00::/7`, `fe80::/10` implicitly by the global-unicast allowlist. Watch IPv4-mapped IPv6 (`::ffff:169.254.169.254`) — normalize with `netip.Addr.Unmap()` before checking.
- Ports: allow only 80 and 443 (blocks aiming delivery at internal Redis/Postgres/SMTP even on public IPs).
- OWASP's caveat applies: "Deny-lists are bypass-prone. Prefer allow-lists" — hence the IPv6 posture above is an allowlist (global unicast only) with targeted denies, not a pure denylist.

### 5.3 DNS rebinding: enforce on the dialed address, not the resolved-then-forgotten one

Validating the URL at registration time (resolve hostname → check IP → save) is insufficient: "an attacker could set up a special DNS server that returns a safe address the first time it's queried, and the target address the second time" (agwa.name). This is a TOCTOU gap — check at registration, use at delivery, DNS answer changed in between (rebinding). OWASP frames the same requirement: validation must apply to "the IP addresses behind the domain name" at request time, A and AAAA records both.

Go gives an exact hook: `net.Dialer.Control` "is called by Go's standard library after the address has been resolved, but before connecting" — the one point where the *actual* IP about to be dialed is known and the connection can still be vetoed:

```go
dialer := &net.Dialer{
    Control: func(network, address string, _ syscall.RawConn) error {
        if network != "tcp4" && network != "tcp6" {
            return fmt.Errorf("ssrf: network %q not allowed", network)
        }
        host, port, err := net.SplitHostPort(address) // not strings.Split: IPv6
        if err != nil { return err }
        if port != "80" && port != "443" {
            return fmt.Errorf("ssrf: port %s not allowed", port)
        }
        addr, err := netip.ParseAddr(host)
        if err != nil { return err }
        if !isAllowedPublic(addr.Unmap()) {
            return fmt.Errorf("ssrf: address %s not allowed", addr)
        }
        return nil
    },
}
transport := &http.Transport{DialContext: dialer.DialContext}
```

Properties of this placement: it fires for **every** connection the client makes — every retry, every redirect hop, every happy-eyeballs candidate address, every DNS re-resolution — so rebinding cannot slip a private IP past a one-time check. `code.dny.dev/ssrf` packages exactly this (`Guardian.Safe` as the Control func, defaults: tcp4/tcp6 only, ports 80/443 only, IANA-synced deny prefixes, `WithAllowedV4Prefixes(...)` overrides for dev). Note `net.IP.IsPrivate()` alone is *not* enough — it covers only RFC1918/RFC4193, missing loopback, link-local, and the metadata range.

Registration-time URL validation is still worth doing (scheme allowlist, resolve-and-check) — but as UX (fail fast with a clear error) and defense-in-depth, never as the enforcement point.

### 5.4 Redirect policy interaction

Redirects are a classic dial-time-check bypass *only when the check is not at dial time*: a public URL 302s to `http://169.254.169.254/`. OWASP recommends "disable the support for the following of the redirection in your web client." With the Control-func approach the redirect hop would be blocked anyway (new connection → Control fires again), but disabling redirects is still the right default for a webhook sender for a non-SSRF reason too: a 3xx is a *response from the wrong place* — the endpoint owner should update the registered URL, and silently following hides misconfiguration and complicates delivery-log semantics. In Go:

```go
client := &http.Client{
    Transport: transport,
    CheckRedirect: func(req *http.Request, via []*http.Request) error {
        return http.ErrUseLastResponse // report the 3xx as the delivery outcome
    },
}
```

### 5.5 Residual risks

Even with dial-time checks, herald's egress can reach anything *public* the operator's network can — it can be used to spam third parties or probe public hosts. Mitigations are rate limits per tenant/endpoint and (for high-assurance deployments) Svix-style network isolation: run delivery workers in a subnet with no route to internal services, so the application-level check is not the only wall.

## 6. Other delivery-side hardening

### 6.1 Payload size caps

Providers cap payloads at the ingest API, not at delivery: GitHub "payloads are capped at 25 MB" (larger events are simply not delivered); Svix accepts up to 1 MiB and recommends keeping payloads under ~40 KB (thin payloads: send an ID, let the consumer fetch details). A cap at POST-message time bounds Postgres row size, memory per delivery worker, and signing cost. For herald v0.1: enforce with `http.MaxBytesReader` on the ingest handler.

### 6.2 HTTPS-only policy (with a dev exception)

Svix: "The most common way to avoid a MITM attack is to always use HTTPS URLs." Without TLS, the payload and the *signature headers* transit in cleartext; a MITM can read payloads (though not forge them — the HMAC still holds — and not usefully replay beyond the tolerance window). Practical policy: scheme allowlist `{https}` by default, with an explicit server-level configuration flag (not per-tenant) permitting `http` for local development — this pairs with the SSRF dev override, since `http://localhost:...` trips both the scheme rule and the loopback deny.

### 6.3 Header injection prevention

If herald ever lets tenants attach custom headers to deliveries (even v0.1's own headers built from IDs): CR/LF in a header name or value is request-splitting. Go's `net/http` client validates header names and values at write time (rejects control characters), which is a real backstop — but herald should still (a) generate `webhook-id` itself (never echo sender-supplied IDs into headers), and (b) if custom headers are added later, allowlist-validate names and reject any tenant attempt to set the reserved `webhook-*` trio, `Host`, `Content-Length`, `Content-Type`, or transfer-encoding headers.

### 6.4 Response handling caps

Delivery logs that store the endpoint's response are an amplification hazard: a hostile endpoint can reply with gigabytes, or trickle bytes forever. Caps: total attempt timeout (GitHub gives receivers 10 seconds to respond with 2xx before "terminates the connection and considers the delivery a failure"; that number is a sane default for herald), and a response-body read cap via `io.LimitReader` (a few KB is plenty for debugging — store truncated body + a truncation flag). Status code and headers are small and safe to log; response bodies are untrusted tenant-adjacent data and must never be rendered unescaped in any UI.

## Comparison table

| Dimension | Standard Webhooks | Stripe | GitHub |
|---|---|---|---|
| Signature header | `webhook-signature` | `Stripe-Signature` | `X-Hub-Signature-256` |
| Timestamp | `webhook-timestamp` header, in signed content | `t=` inside signature header, in signed content | none |
| Message ID in signed content | yes (`webhook-id`) | no (event ID in body only) | no |
| Signed content | `id.timestamp.body` | `timestamp.body` | `body` |
| MAC & encoding | HMAC-SHA256, base64 | HMAC-SHA256, hex | HMAC-SHA256, hex (`sha256=` prefix) |
| Scheme versioning | `v1,`/`v1a,` prefixes, space-delimited list | `v1=`/`v0=` keys, comma list | header name (`-256` suffix) |
| Replay protection | timestamp check, tolerance receiver-defined (5 min de facto) | 5 min default tolerance | none documented |
| Secret format | `whsec_` + base64, 24–64 bytes entropy | `whsec_...` (dashboard-issued) | free-form user string |
| Zero-downtime rotation | yes, multiple sigs in header | yes, old secret ≤24 h | no |
| Asymmetric option | yes, ed25519 (`v1a`, `whpk_`/`whsk_`) | no | no |
| Secret retrievable | implementation-defined (Svix: yes, via API) | yes (dashboard reveal) | no (write-only after set) |

## Gap analysis

- **Standard Webhooks spec leaves the tolerance number open** ("within some allowable tolerance") — herald must pick and document a value rather than inherit one.
- **The spec governs the wire format only**: secret storage, retrievability, rotation window length, SSRF policy, size caps, and HTTPS policy are all sender-implementation decisions the spec is silent on.
- **Sign-per-attempt is implied but easy to miss**: no surveyed doc states outright that senders with long retry backoffs must re-sign each attempt with a fresh timestamp; it falls out of the receiver-side tolerance check. Herald must design the signer into the delivery worker, not the enqueue path.
- **`net.IP.IsPrivate()` gap**: Go's stdlib predicate misses loopback, link-local (metadata!), CGNAT, and special-purpose ranges — a hand-rolled check using it would ship the exact vulnerability it means to prevent.
- **No surveyed provider documents response-body log caps** as a security control; the 10 s GitHub receiver timeout is the closest published number. Herald's choices here (timeout, body cap) will be judgment calls.
- **Svix API secret-retrievability details** were partially inaccessible during research (API reference page did not render); the retrievable-secret claim rests on ecosystem search results and Stripe's dashboard behavior.

## Options

**Option A — Standard Webhooks symmetric scheme (`v1` HMAC-SHA256, base64), headers `webhook-id`/`webhook-timestamp`/`webhook-signature`. ★ RECOMMENDED.**
Why: it is the consolidated best practice of Stripe/Svix-style signing (timestamp + message ID inside the signed content, versioned multi-signature header, typed `whsec_` secrets); consumers can verify with any off-the-shelf standard-webhooks library in their language instead of reading herald docs; it costs nothing extra to implement over a bespoke scheme (one HMAC per active secret per attempt); and its multi-signature header gives rotation for free. Why not: base64-vs-hex and the three-part signed content are marginally more implementation surface than GitHub's body-only HMAC — accepted, because that surface is exactly what buys replay protection and rotation.

**Option B — Stripe-style `t=`/`v1=` single header.** Rejected: functionally close to Option A but nonstandard for anyone who is not Stripe; no message ID in signed content; no library ecosystem for third parties verifying herald deliveries.

**Option C — GitHub-style body-only HMAC (`X-Hub-Signature-256`).** Rejected: no replay protection at all and no rotation story; it survives at GitHub for legacy reasons, not merit.

**Option D — ed25519 asymmetric (`v1a`) in v0.1.** Rejected for v0.1, kept as a compatible future addition: the `v1a` prefix slot means it can be added later without breaking `v1` consumers; v0.1 has no requirement (edge verification, secret-escrow) that justifies key-distribution machinery now.

**Replay window — send fresh timestamp per attempt; document ±5 minutes as the verification tolerance. ★ RECOMMENDED.** Herald signs at delivery-attempt time (fresh `webhook-timestamp`, constant `webhook-id` across retries) so retries beyond any tolerance window still verify; docs tell consumers to enforce ±5 minutes (the Stripe/Svix de-facto number) and to sync clocks with NTP. Alternative (sign once at enqueue) rejected: breaks verification for any retry after the window, which with exponential backoff is most retries.

**Secret rotation — per-endpoint `whsec_` secrets (32 random bytes), rotate API with 24-hour dual-signing overlap. ★ RECOMMENDED.** `endpoint_secrets` rows with `expires_at`; signer emits one space-delimited `v1,` signature per non-expired secret; rotation = insert new + stamp old with `now()+24h`, matching Svix/Stripe windows. Secrets retrievable via a dedicated authenticated `GET /endpoints/{id}/secret` (never in list responses, never logged) — write-only secrets rejected: the consumer must obtain the secret anyway, and write-only merely forces rotations without a security gain. Immediate-expiry rotation (for known compromise) should also be supported, as Stripe does.

**SSRF policy — dial-time enforcement via `net.Dialer.Control` (use or vendor `code.dny.dev/ssrf` defaults), redirects not followed, plus registration-time URL validation as UX. ★ RECOMMENDED.** Control func allows tcp4/tcp6 only, ports 80/443 only, denies all non-public IPv4 ranges and non-global-unicast IPv6 (with `Unmap()` normalization); it runs on every dialed connection, closing the DNS-rebinding TOCTOU. `CheckRedirect` returns `http.ErrUseLastResponse` and the 3xx is recorded as the delivery outcome. A single server-level dev flag relaxes loopback/private denies and the HTTPS-only scheme rule together. Alternatives rejected: registration-time-only validation (rebinding TOCTOU), `net.IP.IsPrivate()`-based checks (misses loopback/link-local/metadata), egress-proxy-only (Smokescreen) as the v0.1 mechanism (right for defense-in-depth in hardened deployments, but herald's "Postgres-only infrastructure, small auditable core" philosophy favors the in-process Control func with the proxy documented as an operator option).

**Delivery hardening bundle. ★ RECOMMENDED** (adopt together): ingest payload cap via `http.MaxBytesReader` (1 MiB hard cap, docs recommending ≤40 KB, matching Svix); HTTPS-only endpoint URLs with the dev-flag exception; herald-generated `webhook-id` (never sender-echoed into headers) and, if custom headers ship later, name/value validation plus a reserved-header denylist; per-attempt timeout 10 s and response-body log cap via `io.LimitReader` (e.g. 4 KB, truncation-flagged).

## Sources

- https://raw.githubusercontent.com/standard-webhooks/standard-webhooks/main/spec/standard-webhooks.md (accessed 2026-07-25) — full Standard Webhooks spec: headers, signed content, `v1`/`v1a` schemes, secret formats, rotation, verification safeguards.
- https://raw.githubusercontent.com/standard-webhooks/standard-webhooks/main/README.md (accessed 2026-07-25) — project rationale, fragmentation problem statement, steering committee and adopters.
- https://docs.stripe.com/webhooks (accessed 2026-07-25) — Stripe signed_payload construction, header format, 5-minute default tolerance, secret roll with ≤24 h delayed expiry, dashboard secret reveal.
- https://docs.stripe.com/webhooks/signature (accessed 2026-07-25) — `Stripe-Signature` `t=`/`v1=`/`v0=` layout and raw-body requirement.
- https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries (accessed 2026-07-25) — `X-Hub-Signature-256` scheme, hex HMAC, constant-time compare, absence of replay protection.
- https://docs.svix.com/receiving/verifying-payloads/how (accessed 2026-07-25) — `svix-*` header aliases, `whsec_` secrets, raw-body warning, `v1,` signature format.
- https://docs.svix.com/receiving/verifying-payloads/why (accessed 2026-07-25) — five-minute two-sided timestamp rejection window, NTP recommendation, replay-attack rationale.
- https://docs.svix.com/security (accessed 2026-07-25) — sender-side SSRF framing, Smokescreen/network-isolation mitigations, HTTPS-for-MITM guidance.
- https://www.svix.com/blog/zero-downtime-secret-rotation-webhooks/ (accessed 2026-07-25) — dual-signing rotation mechanism and space-delimited multi-signature header behavior.
- https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html (accessed 2026-07-25) — deny ranges incl. metadata endpoints, allow-list preference, A+AAAA validation, disable-redirects guidance.
- https://www.agwa.name/blog/post/preventing_server_side_request_forgery_in_golang (accessed 2026-07-25) — DNS-rebinding TOCTOU explanation and the `net.Dialer.Control` post-resolution/pre-connect hook, IPv6-mapped caveat.
- https://pkg.go.dev/code.dny.dev/ssrf (accessed 2026-07-25) — Guardian Control-func package: default tcp4/tcp6 + ports 80/443, full IANA-derived IPv4/IPv6 deny prefixes, allow/deny options, http.Transport wiring.
- https://docs.github.com/en/webhooks/webhook-events-and-payloads (accessed 2026-07-25) — 25 MB payload cap (larger events not delivered).
- https://docs.github.com/en/webhooks/using-webhooks/handling-webhook-deliveries (accessed 2026-07-25) — 10-second receiver response deadline before delivery is failed.

**Unverified claims flagged:** the Svix API reference page (api.svix.com/docs) did not render during research, so the claims that Svix exposes get-endpoint-secret and rotate operations, that a rotated secret "will remain valid for the next 24 hours," and the 1 MiB payload / ~40 KB recommendation figures come from search-result summaries of Svix documentation and blog posts rather than a directly fetched primary page — directionally solid (consistent with Stripe's documented ≤24 h window and the fetched rotation blog post) but the exact API paths and figures should be re-checked against docs.svix.com before being cited in herald docs. The Standard Webhooks 24–64-byte secret-entropy range and `whpk_`/`whsk_` prefixes were extracted from the spec by an automated summarizer and quoted here as reported; exact spec wording should be re-verified before copying into user-facing documentation. The statement that Go's net/http client rejects control characters in header values at write time reflects stdlib behavior known from training rather than a fetched source. All other quoted figures (Stripe 5-minute tolerance, Svix ±5-minute rejection, GitHub 25 MB cap and 10 s timeout, OWASP deny ranges, `code.dny.dev/ssrf` defaults) were taken from directly fetched pages listed above.
