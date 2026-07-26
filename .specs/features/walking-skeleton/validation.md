# Validation — walking-skeleton

**Verdict: PASS** — 0 surviving mutants (8/8 killed), all gates green, no P1–P4 acceptance criterion is a GAP.

Diff range verified: `bc63d94..5a5de9f` (10 commits, 33 files, +6460/-1).

Independence note: this validation was performed by a verifier independent of the authoring phases. Coverage was re-derived from `spec.md` alone — a requirement/AC checklist was built from the spec before any test file was read, and every verdict below points at an assertion, not at a test name or commit message.

## Gates

| Gate | Result |
|---|---|
| `make build` | PASS |
| `make gate-quick` (`go test -race ./...`) | PASS |
| `make gate-full` (`-tags=integration`, Docker up) | PASS |
| `make lint` (golangci-lint v2.12.2) | PASS — 0 issues |
| `make vulncheck` | excluded this cycle (known stdlib advisories on local toolchain) |
| post-mutation `git status --short` | ` M .specs/STATE.md` / ` M SKILLS.md` / `?? .specs/features/` — identical to pre-verification state; no mutation residue |

## Per-requirement evidence

Paths relative to repo root. "kills" = the assertion fails if the behavior is broken, per the mutation runs below or by direct reading of the asserted value.

| Req | Status | Evidence |
|---|---|---|
| SKEL-01 | COVERED | `internal/httpapi/handlers_integration_test.go:102-155` — 201 with `hrld_live_`-prefixed full-scope key; the key then authenticates a real request; DB probed for plaintext (0 rows). `:157-172` — wrong/missing token → 401 and `tenants`/`api_keys` both 0 rows. `internal/httpapi/auth_test.go:89-111` — unset token means nobody bootstraps, not everybody. |
| SKEL-02 | COVERED | `internal/herald/apikey_test.go:78-93` — stored hash equals SHA-256 of issued plaintext; `:51-76` — reflective sweep of every string/[]byte field for the secret; `internal/store/store_integration_test.go:95-142` — DB row holds hash+prefix, neither contains the secret; `internal/migrate/migrate_integration_test.go:174-208` — `api_keys` has exactly the hash/prefix columns (nowhere to store plaintext) and duplicate hashes are rejected. |
| SKEL-03 | COVERED | `internal/httpapi/httpapi_integration_test.go:116-148` — resolved `caller.tenantID`/`scope` equal the issuing tenant's, for two tenants; `:150-178` — unissued/truncated/extended keys → 401, handler never reached; `auth_test.go:14-63` — unparseable credentials refused before any store lookup (nil store would panic). |
| SKEL-04 | COVERED | `handlers_integration_test.go:174-220` — app resolves under the key's tenant; same uid in-tenant → 409; same uid cross-tenant → distinct rows. |
| SKEL-05 | COVERED | `handlers_integration_test.go:187-204` — 201 with filter_types, not disabled, hangs off the right application; `:224-259` — absent/empty/null filters all persist as match-all (proved via `EndpointsForEvent` on an arbitrary type); `:399-437` — `ftp://` URL and empty filter entry → 422 naming the field, 0 rows written. |
| SKEL-06 | COVERED | `handlers_integration_test.go:264-309` — tenant B on tenant A's app uid → 404 `not_found` for endpoint-create and message-read, nothing written, and the owner still gets 200 (route provably works); `:368-397` — ingest key on management → 403, none/unissued → 401, 0 apps created; `internal/httpapi/ingest_integration_test.go:291-309, 312-344`. |
| SKEL-07 | COVERED | `internal/store/ingest_integration_test.go:101-189` — nothing visible in `messages`/`deliveries`/`drover_jobs` pre-commit, exact counts post-commit, every job's args name an existing delivery; `:193-239` — mid-tx failure + rollback → all three tables 0; `internal/httpapi/ingest_integration_test.go:199-236` — DB-trigger fault injected after message+first delivery are written → 500 and zero rows through the real handler; `store/ingest_integration_test.go:361-385` — the store's tx object is the very tx the queue writes through (`var _ pgx.Tx = tx` + visibility check). |
| SKEL-08 | COVERED | `internal/store/store_integration_test.go:226-264` — exact matched-set equality across filtered/unfiltered/disabled endpoints for three event types (NULL matches all; disabled excluded even when matching); `:266-292` — other application's endpoints excluded; `internal/httpapi/ingest_integration_test.go:86-154` — same rule through the API with delivery+job pairing. |
| SKEL-09 | COVERED | `internal/httpapi/server_test.go:62-108` — over-cap body → 413 before the handler reads it; within-cap body arrives whole; non-JSON → 422; `ingest_integration_test.go:238-257` (413 + nothing ingested), `:259-289` (malformed/missing fields → 422 + nothing ingested). `maxBodyBytes = 1 << 20` verified by inspection at `internal/httpapi/server.go:28` (see G1). |
| SKEL-10 | COVERED | `internal/deliver/deliver_test.go:106-135` — receiver-observed method POST, `application/json`, byte-equal payload; `deliver_integration_test.go:173-224` — exactly one attempt row with status 200, snippet, no error, tenant-stamped; `:229-276` — 500 and connection-refused each leave exactly one row (status 500 vs nil-status+error text); `:347-378` — second execution appends attempt #2, does not overwrite #1; `:318-343` — worker re-reads endpoint URL at run time (D-5). |
| SKEL-11 | COVERED | `deliver_test.go:59-102` — table over 200/201/204/299/400/404/500 asserting success flag per status; `:139-160` — 302 recorded, not followed, not success; `deliver_integration_test.go:173-224` — 2xx → status `delivered`, `Work` returns nil; `:229-276` — non-2xx/unreachable → `failed`, `Work` returns error; `internal/e2e/e2e_integration_test.go:456-464` — failed delivery's drover job lands `dead`, delivered one `completed` (queue never told success). |
| SKEL-12 | COVERED | `handlers_integration_test.go:311-366` — GET returns message id/event_type/payload, its delivery (id, endpoint_id, status) and attempt (number, 502, success=false, snippet, duration_ms); `e2e_integration_test.go:305-384` — same read after a real delivery. |
| SKEL-13 | COVERED | `migrate_integration_test.go:113-133` — exact table set incl. unused `endpoint_secrets`; `:135-155` — idempotent re-run; `:160-171` — every tenant-owned table carries `tenant_id`; `:212-331` — CHECK/unique constraints (empty filter list rejected, one delivery per message+endpoint, attempts accumulate, undocumented scope/status rejected). |
| P1 AC1–3 | COVERED | see SKEL-01/02. |
| P2 AC1–4 | COVERED | see SKEL-03..06. |
| P3 AC1–5 | COVERED | AC5: `ingest_integration_test.go:156-184` — no matching endpoint → 202, message stored, 0 deliveries, 0 jobs. Others: SKEL-07/08/09. |
| P4 AC1 | PARTIAL | POST/record aspects fully covered (SKEL-10). The **15s** timeout is asserted only as `client.Timeout == DefaultTimeout` (`deliver_test.go:215-226`), which mirrors the implementation constant; no assertion pins the value to 15 seconds. `DefaultTimeout = 15 * time.Second` verified by inspection (`internal/deliver/deliver.go:33`). See G1. |
| P4 AC2–4 | COVERED | see SKEL-11/12/06; e2e job-state checks close the "not report success to drover" clause. |

Edge cases (spec `spec.md:86-92`): all covered — 500 receiver and unreachable receiver (`deliver_integration_test.go:229-276`), snippet truncation without outcome change (`deliver_test.go:230-266`), two-endpoint independence (`deliver_integration_test.go:280-314`, `e2e_integration_test.go:389-465`), unknown app uid → 404 nothing stored (`ingest_integration_test.go:291-309`).

Spec-precision note (not a gap): the spec says "capped response snippet" without naming the cap; tests assert truncation at the implementation's `snippetBytes` (4 KiB, consistent with ADR-0006). The testable outcome — truncated snippet, outcome unaffected — is asserted.

## Discrimination sensor

Each fault was injected into production code only, run against the narrowest relevant package, and fully reverted before the next.

| # | Target | Mutation | Result |
|---|---|---|---|
| M1 | I-WS-4 | `apikey.go`: store the full plaintext as the display prefix | KILLED — `apikey_test.go:73` ("stored field Prefix holds the key secret") and `:108` |
| M2 | SKEL-11 | `deliver.go`: success window `< 300` → `< 400` | KILLED — `deliver_test.go:152` ("a redirect was treated as a delivery") |
| M3 | SKEL-11 | `deliver.go`: delivery status always set `delivered` | KILLED — `deliver_integration_test.go:255` (both failure modes: "delivery status = delivered, want failed") |
| M4 | I-WS-3 | `deliver.go`: attempt row written only on success | KILLED — `deliver_integration_test.go:259` ("attempts = 0, want exactly 1") |
| M5 | SKEL-08 | `store/applications.go`: drop `AND NOT disabled` from fan-out | KILLED — `store_integration_test.go:252,262` (disabled endpoints selected) |
| M6 | SKEL-08 | `store/applications.go`: drop `filter_types IS NULL OR` (NULL no longer matches all) | KILLED — `store_integration_test.go:252,262` (unfiltered endpoint missing) |
| M7 | I-WS-2 | `store/messages.go`: `DeliveriesByMessage` tenant predicate neutralized | KILLED — `store_integration_test.go:339` ("another tenant saw 1 deliveries, want 0") |
| M8 | I-WS-1 | `httpapi/ingest.go`: commit message+deliveries without enqueuing jobs | KILLED — `ingest_integration_test.go:132` ("queued jobs = 0, want one per delivery (2)") and `:332` |

Surviving mutants: 0.

## Gaps

- **G1 (low)** — The spec-valued constants (1 MiB body cap, 15s delivery timeout) are asserted only relative to the implementation constants (`maxBodyBytes`, `DefaultTimeout`); a change to either constant would survive the suite. The values are correct today (`internal/httpapi/server.go:28`, `internal/deliver/deliver.go:33`). Fix-task: add unit assertions pinning `DefaultTimeout == 15*time.Second` and `maxBodyBytes == 1<<20` (or drive the cap test with a literal `1<<20` body). Does not gate: P4 AC1 is PARTIAL, not GAP.
