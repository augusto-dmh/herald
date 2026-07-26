---
name: pr-review
description: 'Multi-agent PR reviewer for Herald. Use when — and only when — explicitly asked to review a pull request: "review PR #N", "review this PR", "code review this PR", "check this pull request". Not for automatic use during coding, feature implementation, or finalizing/publishing (use herald-finalize), nor for general questions.'
license: CC-BY-4.0
metadata:
  author: Herald contributors
  version: 1.0.0
---

# PR Review — Orchestration Protocol

## Project facts

Porting this skill to another project means editing only this section (and re-targeting the lane content to the new stack).

- **Repo:** `augusto-dmh/herald` — a self-hosted multi-tenant webhook delivery service, built on the drover task-queue library. Default branch: `main`.
- **Language:** Go. Module path: `github.com/augusto-dmh/herald`.
- **Gates:** the root `Makefile` is the single source of truth for gate commands — this skill references target names only. `make gate-quick` (unit tests with the race detector) · `make gate-full` (gate-quick plus integration tests, which use testcontainers — probe `docker ps` first) · `make build` (compile + vet) · `make lint` (pinned golangci-lint).
- **Docs:** `docs/adr/` (accepted decisions), `docs/rfc/` (proposals; the roadmap RFC — RFC-0001 for v0.1 — defines cycle scope), `docs/research/YYYY-MM-DD/` (durable research).
- **Working state:** `.specs/STATE.md`, `.specs/ROADMAP.md`, `.specs/features/<cycle>/`, heartbeat `.specs/.ship-status` (gitignored).

## Protocol

Coordinates 6 specialized subagents (via the Agent tool) then consolidates findings into a unified summary. Each subagent loads the relevant existing Herald docs (`CLAUDE.md`, `docs/adr/`, the roadmap RFC, `.specs/`) — this skill does not duplicate them.

Herald stack: a Go service (project facts above) that ingests events, fans them out per tenant to subscribed endpoints, and delivers HMAC-signed outbound HTTP requests through drover-backed workers with retries — never inside request handlers. PostgreSQL is the source of truth for tenants, endpoints, subscriptions, and delivery state. `CLAUDE.md` and `docs/adr/` are authoritative for the architecture; do not review against constraints they do not state.

## Step 1: Initialize

1. Get PR number from context or ask the user.
2. Identify repo: `gh repo view --json nameWithOwner -q .nameWithOwner`
3. Fetch diff: `gh pr diff {PR_NUMBER}` — save it to a scratchpad file for the subagents. If it exceeds ~200KB, split it per-file or per-directory (`split -b 200k` as a last resort) and hand subagents the chunk list: a Read of a 256KB+ file fails, and six subagents each burning that failed Read is the observed cost.
4. Load existing inline comments: `gh api repos/{REPO}/pulls/{PR_NUMBER}/comments` — build a set of `{path, line}` pairs to avoid reposting.
5. Read PR intent: `gh pr view {PR_NUMBER} --json title,body,headRefName`
6. Derive the feature slug from the branch name: strip the Conventional Commit prefix (`feat/`, `fix/`, `docs/`, …) and any leading issue number, leaving a kebab summary (e.g. `feat/endpoint-registration` → `endpoint-registration`). This slug is the fuzzy key for locating the matching spec under `.specs/features/`.

## Step 2: Launch Subagents in Parallel

Send **one message** with **six Agent tool calls** — all launched simultaneously. Pass REPO, PR_NUMBER, the diff, existing comment locations, the PR intent, and the feature slug to each subagent prompt. After all complete, run Step 3.

---

## Severity Labels (all subagents use these)

- 🚨 Critical — bugs or logic errors that will cause failures
- 🔒 Security — security vulnerabilities or data exposure
- ⚡ Performance — significant performance concerns
- ⚠️ Warning — code smells or maintainability issues
- 💡 Suggestion — optional improvements

---

## Universal Rules (every subagent must follow)

1. **Comment allowlist:** Only post inline comments on lines in the diff starting with `+` (excluding `+++`).
2. **Skip duplicates:** If `{path, line}` within ±3 lines already has a comment, skip.
3. **Mark resolved:** Reply `[RESOLVED] This appears resolved by the recent changes.` on existing comments where the issue is fixed.
4. **False positive guard:** Only report findings with ≥80% confidence. Skip when uncertain.
5. **Positive highlight:** Include at least one well-done aspect of the change before listing issues.
6. **Tone:** Specific, actionable, collegial. Explain WHY something is a problem, and cite the ADR / CLAUDE.md durable decision / Go convention that grounds it.
7. **Never** approve, request-changes, or modify files. Comments only.
8. **Marker:** Start every inline comment body with `<!-- herald-review:{type} -->` (invisible in rendered view, used by the consolidation subagent).
9. **No AI attribution:** Never add tooling/authorship attribution (`Co-Authored-By`, "Generated with", model names) to any comment body — consistent with `herald-finalize` hygiene rules.
10. **Multiline bodies from a file:** Write the comment body to a temp file, then post with `gh pr comment --body-file <file>` or `gh api ... -F body=@<file>`. Never use `gh api -f body=@<file>` — lowercase `-f` does not expand `@`, so the comment is published as the literal file path instead of its content.
11. **PR-level comments must be issue comments, never reviews.** Post every PR-level comment (requirements summary, consolidation summary) with `gh pr comment` / `gh api .../issues/comments`, not `gh pr review`. Submitted reviews cannot be deleted, dismissed, or blanked via the API, so they leave permanent artifacts that break teardown (Step 4).
12. **Oversized inputs:** never Read the shared diff file whole when it may exceed the 256KB cap — use the per-file chunks from Step 1, or `offset`/`limit`/grep.
13. **Completion contract:** a subagent's work ends only when its findings are posted to the PR (or its summary comment updated) and a compact final text summary is returned. Never go idle without posting — an idle with nothing posted is treated as a stall: the orchestrator checks comment counts, nudges once, then re-dispatches a fresh subagent.

---

## Structural Compliance Checklist

Ground each item in `CLAUDE.md`'s durable decisions and the accepted ADRs under `docs/adr/` — cite the specific document, not this list:

- [ ] **Tenant isolation** — every query and handler touching tenant-owned resources (endpoints, subscriptions, deliveries, secrets) is scoped by tenant; no path returns or mutates another tenant's data.
- [ ] **Workers, not request handlers** — outbound delivery, retries, and other long-running work run through drover-backed workers, not inside HTTP request handlers.
- [ ] **Signing and secrets** — outbound requests are HMAC-signed per the documented scheme; secrets are env/config-sourced and never logged or exposed in responses.
- [ ] **Postgres as source of truth** — durable tenant, subscription, and delivery state lives in PostgreSQL behind the documented storage boundary; no shadow state.
- [ ] **Drover boundary** — herald consumes drover through its public API; no reach-in to drover internals or reimplementation of queue semantics drover already owns.
- [ ] **Scope walls (roadmap RFC)** — the change stays inside the current cycle's scope; deferred capabilities are not smuggled in.

---

## Subagent 1: Security

**Marker:** `<!-- herald-review:security -->`

Load `CLAUDE.md` and skim the ADRs under `docs/adr/` that touch auth, tenancy, signing, or delivery. Review the PR diff for: hardcoded secrets or API keys (must be env/config-only); **SSRF on outbound requests** — endpoint URLs must be validated before delivery (scheme allowlist, private/link-local/metadata IP ranges blocked, redirects not followed blindly, DNS-rebinding considered); **HMAC signing and secret handling** — correct algorithm use, constant-time comparison on any verification path, per-tenant/per-endpoint secrets never shared, logged, or returned in responses; **replay windows** — signed timestamps with a bounded tolerance, and verification that rejects stale or reused signatures; **tenant isolation / IDOR** — every tenant-scoped query filtered by the authenticated tenant, no resource lookup by bare ID that skips the ownership predicate; missing auth middleware/checks on new endpoints; raw SQL string concatenation instead of parameterized queries; PII, payloads, or secrets in logs; sensitive fields leaking into API responses; and overly permissive CORS or admin surfaces.

**Second pass:** Re-read the full diff from top to bottom. List every file or hunk you did not comment on. For each uncovered file, ask: "Does this file violate any security rule in my scope?" Only skip a file when you can explicitly state why it is clean.

**Comment format:**
```
<!-- herald-review:security -->
🔒 Security — [Short title]
[What the issue is and why it matters]
**Recommendation:** [Specific fix]
```

---

## Subagent 2: Requirements & Definition of Done

**Marker:** `<!-- herald-review:requirements -->`
**Posts:** One PR-level summary comment only — no inline comments.

Use a two-track approach to find requirements. Run both tracks; use whichever yields content.

### Track A — Feature Spec (`.specs/features/`)

1. Use the feature slug derived in Step 1. Look for `.specs/features/{slug}/` — if an exact match is absent, fuzzy-match the slug against directory stems under `.specs/features/`, and also check the PR title/body for an explicit spec path or markdown link.
2. For the matched feature, read `spec.md`, `tasks.md`, and `validation.md` with `cat {path}`.
3. Extract: functional requirements (`FR-*` IDs), acceptance criteria, the task checklist, and stated goals / out-of-scope items.

### Track B — Accepted Decisions (ADR / RFC)

1. Scan the PR title, body, and matched spec for referenced decisions (`ADR-0NNN`, `RFC-0NNN`) under `docs/adr/` and `docs/rfc/`; the roadmap RFC (project facts) defines the cycle's scope.
2. Read each referenced doc and extract the constraints the PR must honor (delivery semantics, signing scheme, tenancy rules, storage boundaries).

### Resolution Logic

| Tracks with content | Action |
|---|---|
| Both A and B | Merge requirements from both; note the source of each item (spec FR-ID or ADR/RFC number) |
| A only | Use the feature spec requirements |
| B only | Use the ADR/RFC constraints |
| Neither | Post: "⚠️ No matching `.specs/features/` spec or referenced ADR/RFC found — requirements verification skipped." and stop |

Compare the merged requirements against the PR diff and post the summary **idempotently as an issue comment** (never a review — see Step 3.8). Look for an existing PR comment containing `<!-- herald-review:requirements -->`: if one exists, update it in place with `gh api -X PATCH repos/{REPO}/issues/comments/{COMMENT_ID} -F body=@<tempfile>`; otherwise create it with `gh pr comment {PR_NUMBER} --body-file <tempfile>`. This keeps the comment editable and removable, and prevents duplicate requirements comments across re-runs.

**Second pass:** After drafting the summary, re-read the requirements list one item at a time and ask: "Did I evaluate this criterion against the diff?" For any item not yet assessed, find the relevant section of the diff and explicitly mark it ✅, ❌, or 🔲.

**Summary format:**
```markdown
<!-- herald-review:requirements -->
## 📋 Requirements Review

**Sources:** {e.g. "Spec: .specs/features/endpoint-registration" · "ADR-0002, roadmap RFC" · "Both"}

### ✅ Implemented
### ❌ Missing or Incomplete
### 🔲 Definition of Done
- [x] covered  - [ ] not covered
### 💬 Notes
```

---

## Subagent 3: Test Coverage

**Marker:** `<!-- herald-review:tests -->`

Load the Workflow/testing sections of `CLAUDE.md`. Herald testing: Go table-driven tests with the race detector always on (`make gate-quick`); the unit suite must run without Docker; integration tests use testcontainers behind the `integration` build tag (`make gate-full`, requires Docker). The Makefile is the single source of truth for gate commands.

Review the PR diff for: new or changed HTTP handlers, storage methods, delivery/worker logic, or exported API with no covering test (🚨 Critical for new handlers and state transitions); unit tests that secretly require Docker (breaking the Docker-free unit contract); missing tenant-isolation or authorization test cases on tenant-scoped resources; missing coverage of signing/verification and retry/failure paths where the spec lists them; assertion-quality issues (asserting only that no error occurred when a value or state is the contract, asserting mock calls instead of resulting state, hardcoded IDs, missing DB cleanup/isolation); and anti-patterns (asserting only status codes, no error/edge case, tests that would pass under a plausibly wrong implementation, weakened or deleted assertions).

**Second pass:** Re-read the full diff from top to bottom. List every new or modified handler, storage method, and worker/delivery path you did not comment on. For each uncovered one, ask: "Is there a test covering the happy path, at least one failure path, and the tenant/authorization dimension if it has one?" Only skip when you can explicitly state why coverage already exists or is not applicable.

**Comment format:**
```
<!-- herald-review:tests -->
[🚨/⚠️/💡] — [Short title]
[Description of the gap or anti-pattern]
**Recommendation:** [Go test pattern or case to add, grounded in the spec or CLAUDE.md]
```

---

## Subagent 4: Architecture & Coding Patterns

**Marker:** `<!-- herald-review:architecture -->`

### Phase 0 — Load all reference documents

Load every document below before touching the diff. Do not skip any.

1. `CLAUDE.md`
2. Every ADR under `docs/adr/`
3. The roadmap RFC under `docs/rfc/` (project facts)
4. `.specs/STATE.md`
5. The cycle's `.specs/features/{slug}/spec.md`

Then scan the diff for directory structure: note which packages and layers the changed paths touch and how they sit relative to the documented boundaries (HTTP surface, storage, delivery workers, drover integration).

### Phase 1 — Extract the rule list from the loaded documents

Do not use a hardcoded list. After loading Phase 0, scan each document and extract every explicit rule into a single numbered checklist:

- **`CLAUDE.md`** — extract every durable decision and locked convention (layout, boundaries, tooling, workflow constraints).
- **Each ADR** — extract each binding constraint (stack choices, storage boundary, delivery semantics, signing scheme, tenancy rules).
- **The roadmap RFC** — extract the current cycle's scope and any explicit scope walls.
- **`.specs/STATE.md`** — extract accepted `AD-NNN` decisions the change must conform to.
- **`spec.md`** — extract the cycle's stated constraints and out-of-scope items.

Number the combined list sequentially from 1. This is your evaluation matrix for Phase 2. Do not add rules absent from the documents, and do not omit any you find.

Additionally apply standard Go review judgment where the docs are silent: exported identifiers need doc comments; errors are wrapped with context (`%w`), never swallowed; context-first parameters; consumer-defined interfaces kept small; no naked `any` where a concrete type fits; zero values made useful; no premature abstraction.

### Phase 2 — Evaluate the matrix

Work through the diff **one file at a time**. For each changed file:

- For each rule, decide **PASS** / **VIOLATION** / **N/A**.
- N/A is only valid when the rule is structurally inapplicable to the file type (e.g. a SQL migration cannot violate a goroutine rule; a plain struct-declaration file cannot violate handler leanness).
- For every VIOLATION: post an inline comment on the exact `+` line that is the evidence. Include the rule number and source document.

**Second pass:** After completing the matrix for all files, re-read the full diff top to bottom. List every file or hunk you did not evaluate. For any uncovered file, run the matrix again. Only skip a file when you can explicitly state which rules are N/A and why.

**Comment format:**
```
<!-- herald-review:architecture -->
[🚨/⚠️/💡] — [Short title]
Rule: [Rule number + source, e.g. "Rule 8 — ADR on storage boundary" or "CLAUDE.md durable decision"]
[What in the diff violates it — quote the offending line]
**Recommendation:** [Exact fix, code snippet if < 6 lines]
```

---

## Subagent 5: Regression & Hallucination Detection

**Marker:** `<!-- herald-review:regression -->`

Review the PR diff for changes unrelated to the PR's stated purpose, or signs of AI-generated artifacts. Look for: deleted code unrelated to the change (🚨 Critical), phantom imports referencing non-existent packages/symbols (🚨 Critical), function/method calls with wrong signatures (🚨 Critical), `TODO`/`FIXME`/`panic("not implemented")` stubs left in production code, `//nolint` directives or blank-identifier assignments (`_ = err`) hiding real errors, duplicate logic that already exists in the package or in drover, weakened validation (input checks removed, auth or tenancy guards loosened), silently swallowed errors in handlers or delivery workers, weakened test assertions, and dead code that is never called.

**Second pass:** Re-read the full diff from top to bottom. List every file or hunk you did not comment on. For each uncovered file, ask: "Does this file contain any unrelated deletions, phantom imports, duplicate logic, or weakened assertions?" Only skip a file when you can explicitly state why none of those categories apply.

**Comment format:**
```
<!-- herald-review:regression -->
[🚨/⚠️/💡] — [Short title]
Type: [unrelated-deletion | phantom-import | hallucination | duplicate | regression | dead-code]
[Specific description with quoted evidence from the diff]
**Recommendation:** [Exact fix]
```

---

## Subagent 6: Performance & SQL

**Marker:** `<!-- herald-review:performance -->`

Only flag issues **clearly visible in the diff** — no speculation. Herald's hot paths are event fan-out and outbound HTTP delivery over PostgreSQL-backed state.

**Go:** goroutine leaks (every `go func` needs a termination story); unbounded concurrency (fan-out with no semaphore, worker pool, or bounded queue); missing `context.Context` propagation or cancellation not honored on delivery and storage paths; `http.Client`/`Transport` misuse (per-request client creation, no timeout, default transport where connection reuse or limits matter); response bodies not drained and closed (leaks the connection); blocking work with no deadline inside handlers; allocations in hot paths where a cheap fix is visible (buffer reuse, avoiding per-event marshaling churn).

**PostgreSQL:** N+1 query patterns (a query inside a loop that a batch or join should replace); queries whose predicates cannot use an existing index, or new access paths with no supporting index; locking (`FOR UPDATE` scope, long-open transactions that pin locks or the xmin horizon, contention on hot delivery rows); unbounded queries with no `LIMIT`/pagination; missing transaction boundaries around multi-statement writes; migrations that are not reversible or that lock large tables without a stated strategy; string-concatenated SQL instead of parameterized queries (🔒).

**Second pass:** Re-read the full diff from top to bottom. List every query, transaction, loop, goroutine spawn, and HTTP call you did not comment on. For each uncovered block, ask: "Does this contain a clearly visible performance, locking, or data-safety issue?" Only skip a block when you can explicitly state why none of the patterns above apply.

**Comment format:**
```
<!-- herald-review:performance -->
[⚡/🔒/🚨/⚠️] — [Short title]
[Description with estimated impact, e.g. "O(N) queries per fan-out"]
**Recommendation:** [Fix with short code sketch if < 6 lines]
```

---

## Step 3: Consolidation

After all 6 subagents complete, spawn one more subagent via the Agent tool to consolidate:

1. `gh api repos/{REPO}/pulls/{PR_NUMBER}/comments` — fetch all inline comments.
2. Filter to those starting with `<!-- herald-review:` and parse the type from the marker.
3. Fetch PR-level comments for the `<!-- herald-review:requirements -->` summary.
4. Group by severity: 🔒 Security → 🚨 Critical → ⚡ Performance → ⚠️ Warning → 💡 Suggestion.
5. Deduplicate findings at the same `{path, line}` (±3 lines) — note both agents in the entry.
6. Collect one positive highlight per agent.
7. **Gap detection:** Run `gh pr diff {PR_NUMBER} --name-only` to get the full list of changed files. Cross-reference against all collected inline comment paths. For any file with zero inline comments from any subagent, add it to a `### 🔍 Files With No Inline Comments` section. Omit a file from this section only if it is a config/lock file (`*.json`, `*.yaml`, `*.yml`, `*.toml`, `*.mod`, `*.sum`, `.golangci.yml`, `.env.example`) or a pure declaration/migration file with no logic (a bare SQL migration, a struct-only file).
8. Post the summary **as an issue comment, never as a review**. A submitted `gh pr review` (even `--comment`) creates a review that GitHub's API cannot delete, dismiss, or blank — it is permanent. An issue comment stays editable and removable. Post idempotently by marker: search existing PR comments for `<!-- herald-review:summary -->`; if found, update it in place with `gh api -X PATCH repos/{REPO}/issues/comments/{COMMENT_ID} -F body=@<tempfile>`; otherwise create it with `gh pr comment {PR_NUMBER} --body-file <tempfile>`. This keeps the summary removable and avoids duplicate summaries when the review is re-run.

**Summary format:**
```markdown
<!-- herald-review:summary -->
## 🤖 Herald AI Review Summary

| | |
|---|---|
| **Subagents invoked** | {N} of 6 (Security · Requirements (Spec + ADR/RFC) · Test Coverage · Architecture · Regression · Performance & SQL) |
| **Skills loaded** | `.claude/skills/pr-review/SKILL.md` |
| **Docs loaded** | `CLAUDE.md`, `docs/adr/*`, the roadmap RFC, `.specs/STATE.md`, `.specs/features/{slug}/*` |
| **Findings** | {N} across {M} files |

---

### 🔒 Security ({N})
- [`path/file.go:L42`] Finding title

### 🚨 Critical ({N})
### ⚡ Performance ({N})
### ⚠️ Warnings ({N})
### 💡 Suggestions ({N})

---
### 🔍 Files With No Inline Comments
- `path/to/file.go` — no findings from any subagent (verify manually or re-run targeted review)

_(Omit this section if all logic files received at least one comment.)_

---
### ✅ Highlights
- [One positive highlight per agent]

---
> See inline comments for details and recommendations.
```

If no findings across all agents: post `✅ No issues found across all review dimensions.` but still include the metadata table.

---

## Step 4: Teardown / re-run

The review's artifacts must be fully removable so a re-run never duplicates them and the author can clear the PR. All review output is therefore inline comments and issue comments only (Step 3.8) — never a submitted review.

**Resolve a thread after its finding is fixed.** Reply, then resolve, via GraphQL:

```bash
# Reply in the thread
gh api graphql -f query='mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id}}}' -f t="$THREAD_ID" -f b="Fixed in <hash> — <one line>."
# Resolve it
gh api graphql -f query='mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}' -f t="$THREAD_ID"
```

Thread IDs come from `repository.pullRequest.reviewThreads` (each node has `id`, `isResolved`, and its `comments`).

**Remove all bot comments.** Everything the review posts is deletable:

```bash
# Inline review comments (findings + replies)
for id in $(gh api repos/{REPO}/pulls/{PR_NUMBER}/comments --paginate --jq '.[].id'); do
  gh api -X DELETE repos/{REPO}/pulls/comments/$id
done
# PR-level issue comments (requirements + summary) — filter by marker if selective
for id in $(gh api repos/{REPO}/issues/{PR_NUMBER}/comments --paginate --jq '.[].id'); do
  gh api -X DELETE repos/{REPO}/issues/comments/$id
done
```

**Do not create what you cannot remove.** There is no API to delete or dismiss a `COMMENTED` review, and an empty review body is rejected (HTTP 422). That is why summaries are issue comments — keep it that way.
