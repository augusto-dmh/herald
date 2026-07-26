# Review triage — `walking-skeleton` (PR #1)

24 inline + 2 PR-level comments after the review's own cross-lane dedupe. Verdicts judged against the code at `12f3e06`; comments are deleted after this record per the ship-cycle protocol.

| # | Comment id | Lane | File:line | Finding | Verdict | Action |
|---|---|---|---|---|---|---|
| 1 | 3652474233 | security | deliver.go:210 | Invalid-UTF-8 response bytes make `CreateAttempt` fail (`invalid byte sequence`), breaking I-WS-3 and stranding the delivery `pending`; reachable by an innocent receiver (multibyte char at the 4 KiB cut) | **real** | **fix**: sanitize snippet to valid UTF-8 + strip NULs; test with hostile bytes |
| 2 | 3652474528 | security | deliver.go:203 | Endpoint URLs logged (and embedded in the returned job error); webhook URLs are routinely capability tokens | **real** | **fix**: log/report IDs, never full URLs |
| 3 | 3652473714 | perf | deliver.go:199 | Body never drained; >4 KiB replies burn the connection | **real** | **fix**: drain up to the delivery-contract drain cap (64 KiB) before close — 3 lines, pulls a documented decision forward rather than violating it |
| 4 | 3652474652 | arch | deliver.go:197 | Code comment argues against the drain rule instead of deferring to it | **real** | **fix**: comment corrected by #3's change |
| 5 | 3652472854 | tests | deliver.go:111 | Worker load-failure exit (the documented I-WS-3 exception) has no test | **real** | **fix**: test unknown-delivery job → error, zero attempt rows |
| 6 | 3652472885 | tests | deliver_test.go:97 | `duration < 0` assertion is unfalsifiable (also echoed at e2e:358) | **real** | **fix**: require positive duration, both sites |
| 7 | 3652473067 | tests | deliver.go:159 | `attemptNumber` floor unexercised | **real** | **fix**: unit test both branches |
| 8 | 3652483553 | consolidator | deliver.go:140 | Attempt insert and status update are two independent commits | real | **won't fix**: a crash between them means the job never completed, which under the queue's current no-rescuer semantics strands the job regardless of herald's tx boundary — the real cure is the lease/rescuer + retry cycle, which restructures this exact path; wrapping now buys no durable property |
| 9 | 3652474576 | regression | domain.go:78 | Status doc comment describes the not-yet-built retry ladder | **real** | **fix**: describe current semantics |
| 10 | 3652473844 | regression | tenants.go:41 | `TenantByID` has no callers | **real** | **fix**: delete (trivially restored when a caller exists) |
| 11 | 3652473865 + 3652474625 + 3652474574 | regression/arch/security | tenants.go:102, auth.go:73 | `TouchAPIKey` never called; ADR-0002's throttled `last_used_at` permanently NULL (three lanes independently) | **real** | **fix**: throttled touch on the auth path + test |
| 12 | 3652473089 | tests | messages.go:48 | Non-UUID message id path uncovered | **real** | **fix**: test |
| 13 | 3652473106 | tests | server.go:79 | Endpoint create lacks a scope-enforcement test | **real** | **fix**: test |
| 14 | 3652473362 | perf | ingest.go:76 | Fan-out is 2N sequential round-trips inside the open tx, unbounded per application | real | **won't fix**: correctness-first skeleton; batching is an optimization that the roadmap's load-characterization cycle must justify with measurements (repo rule: no unmeasured performance claims); revisited there |
| 15 | 3652473547 | perf | messages.go:251 | Missing `d.tenant_id` predicate leaves `deliveries_message_idx` unusable | **real** | **fix**: add the predicate — semantically free, restores the index path |
| 16 | 3652474360 | arch | .golangci.yml:16 | Lint exclusion for a generated-code path that the query-layer decision (AD-001) says will never exist | **real** | **fix**: remove the exclusion |
| 17 | 3652474380 | arch | ci.yml:27 | CI inlines commands the Makefile owns, violating the repo's own single-source rule | **real** | **fix**: CI calls make targets (new `gate-integration` target); lint stays on the pinned action for annotations, same version as the Makefile pin |
| 18 | 3652474423 | arch | ci.yml:20 | `go 1.26.2` directive makes the 1.25.x matrix leg re-run 1.26.2 — one toolchain tested twice | **real** | **fix**: single-version matrix |
| 19 | 3652474554 | security | deliver.go:93 | SSRF surface open until the egress-hardening cycle | real | **won't fix**: explicitly deferred by the roadmap's cycle walls; the lane itself notes, not objects |
| 20 | 3652474606 | regression | applications.go:102 | Three non-Tx store methods serve only test fixtures | real | **won't fix**: they are the natural public store shape; handlers use Tx variants exactly where atomicity is an invariant, and removing the plain forms couples every test to transaction boilerplate |
| 21 | 3652483629 | consolidator | 0001_initial_schema.sql:89 | `payload jsonb` silently rewrites tenant bytes (key order, whitespace, duplicate keys) | **real** | **fix**: store payload as `bytea` (JSON still validated at ingest); byte fidelity is a prerequisite for the signing cycle — signatures must cover the bytes the tenant sent. Recorded as AD-009 |
| 22 | 3652483219 | consolidator | e2e_integration_test.go:374 | Payload equality asserted only semantically, masking normalization | **real** | **fix**: byte-for-byte receiver-side assertion (enabled by #21) |
| 23 | 5083463370 | requirements (PR-level) | — | Two ❌: `last_used_at` never written (→ #11); shipped `validation.md` still lists G1 open though a later commit closed it | **real** | **fix**: amend validation.md addendum |
| 24 | 5083492606 | consolidator (PR-level) | — | Consolidated summary | informational | none |

Unposted lane note adopted: rename `TestWithNoBootstrapTokenConfiguredNoTokenOpensTheRoute` — the name reads opposite to what it (correctly) asserts; in a fail-closed auth test a misread name invites a wrong "fix".

**Tally: 19 real-fix (across 17 rows), 4 real-won't-fix, 1 informational.** No finding rejected as false — the review misread nothing.
