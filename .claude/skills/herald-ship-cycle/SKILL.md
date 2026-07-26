---
name: herald-ship-cycle
description: 'End-to-end orchestrator for one Herald roadmap PR: pick the next cycle from the roadmap, run a tlc-spec-driven cycle auto-selecting recommended options, publish the PR with herald-finalize, run pr-review in a fresh-context subagent, triage every review finding against the code, apply accepted fixes, delete all PR comments, and merge after a single user approval. Use when asked to "ship the next PR", "run the ship cycle", "do the next roadmap cycle end-to-end", or to resume a partially shipped cycle. Not for ad-hoc edits, standalone reviews (use pr-review), or publishing-only work (use herald-finalize).'
license: CC-BY-4.0
metadata:
  author: Herald contributors
  version: 1.0.0
---

# Herald Ship Cycle — Orchestration Protocol

## Project facts

Porting this skill to another project means editing only this section.

- **Repo:** `augusto-dmh/herald` — a self-hosted multi-tenant webhook delivery service, built on the drover task-queue library. Default branch: `main`.
- **Language:** Go. Module path: `github.com/augusto-dmh/herald`.
- **Gates:** the root `Makefile` is the single source of truth for gate commands — this skill references target names only. `make gate-quick` (unit tests with the race detector) · `make gate-full` (gate-quick plus integration tests, which use testcontainers — probe `docker ps` first and tell the user to start Docker Desktop rather than debugging further) · `make build` (compile + vet) · `make lint` (pinned golangci-lint). Pre-publish gate: `make build && make gate-full && make lint`.
- **Docs:** `docs/adr/` (accepted decisions), `docs/rfc/` (proposals; the roadmap RFC — RFC-0001 for v0.1 — defines cycle scope), `docs/research/YYYY-MM-DD/` (durable research).
- **Working state:** `.specs/STATE.md` (decision log + handoff), `.specs/ROADMAP.md` (cycle table), `.specs/features/<cycle>/` (cycle artifacts), heartbeat `.specs/.ship-status` (gitignored).

## Protocol

Runs one roadmap cycle from "what's the next PR?" to merged, replacing three manual sessions (tlc build → pr-review → triage/cleanup/merge) with one orchestrated pipeline. This skill owns only the glue; the work itself is delegated to `tlc-spec-driven`, `herald-finalize`, and `pr-review` unchanged.

**Autonomy contract:** the pipeline runs without user prompts except at exactly one gate — merge approval (Stage 7) — plus the escalation rule in Stage 1. Everything else proceeds on the recommended option, logged for audit.

**Continuation contract (binding):** never end a turn "standing by", "awaiting your call", or asking "want me to proceed?" between stages — finish the stage, update the heartbeat, and start the next stage in the same turn. Anything that needs the user's eyes (e.g. a manual delivery check) is deferred to the Stage 7 report, not raised as a mid-cycle pause. The only allowed stops are: the Stage 7 gate, a Verifier FAIL, and a hard blocker only the user can clear (state which one when stopping).

**Heartbeat (`.specs/.ship-status`):** at every stage transition, overwrite `.specs/.ship-status` with a single line: `<cycle> | Stage <N> <name> | <branch or PR #> | <timestamp> | <one-line status>`. This is how the user (or a parallel session) answers "what is going on?" without archaeology. Also update it when parking on an outage or limit. Delete the file at Stage 8.

**Resume contract:** on any session start or post-interruption message (a bare "continue" suffices — do not ask what to do), read `.specs/.ship-status`; if it shows a mid-stage cycle, run Stage Detection and resume immediately in that same turn.

## Run modes (optional skill argument)

- `once` (default) — stop at the Stage 7 merge gate for the single approval.
- `auto` — this invocation is the user's standing merge approval: when the Stage 7 report is clean (Verifier PASS, gates green, comments cleaned), merge without asking, run Stage 8, and start the next cycle. Still stop for a Verifier FAIL, an escalation-rule decision, or a dirty report.
- `until <roadmap row>` — like `auto`, but stop after the named row merges.

The mode holds for the whole invocation; do not re-ask it mid-run.

## Stage Detection (always run first)

The pipeline is resumable. Check `.specs/.ship-status` first — if it shows an in-flight cycle (possibly from another session), resume that cycle rather than starting a new one; two concurrent cycles on one repo is always a mistake. Then determine the current stage:

| Observation | Resume at |
|---|---|
| Clean `main`, first ROADMAP row not marked Done has no branch yet | Stage 0 |
| Cycle branch exists, tlc Execute/Verifier incomplete (`.specs/features/<cycle>/`) | Stage 1 (tlc resume) |
| Verifier PASS on branch, no open PR | Stage 2 |
| PR open, no `<!-- herald-review:` comments on it | Stage 3 |
| PR has review comments, no `.specs/features/<cycle>/review-triage.md` | Stage 4 |
| `review-triage.md` exists, accepted fixes not yet pushed | Stage 5 |
| Fixes pushed, PR comments still present | Stage 6 |
| PR comment-free, unmerged | Stage 7 |

Announce the detected stage and the cycle/PR it applies to before proceeding.

## Stage 0 — Preflight

1. Require a clean working tree. If dirty, stop and report — never stash or discard.
2. `git checkout main && git pull`.
3. Read `.specs/ROADMAP.md` and the Handoff section of `.specs/STATE.md`. The next cycle is the first ROADMAP.md row not marked Done; its scope comes from the corresponding section of the roadmap RFC (project facts above).
4. State the chosen cycle, its roadmap-RFC section, and the intended slice in one short paragraph, then continue — no approval needed.

## Stage 1 — Plan & Build (tlc-spec-driven)

Invoke `tlc-spec-driven` for the cycle (Specify → Design → Tasks → Execute per its auto-sizing).

**Auto-decision rule** (replaces the human answering Discuss questions): at every decision point, formulate the options — each with why-recommend AND why-not — pick the recommended one, and record option set, choice, and rationale in the cycle's `context.md` and as an `AD-NNN` row in `.specs/STATE.md`. The decision must be auditable later without the conversation.

**Escalation rule** — ask the user (AskUserQuestion) instead of auto-deciding only when:
- the decision changes product direction or roadmap scope beyond the cycle,
- it locks in an external dependency/provider that CLAUDE.md or an ADR says needs its own decision, and no clear recommendation exists, or
- no option is defensible as recommended.

Execute honors the full tlc contract (tests from acceptance criteria, gate per task, atomic commits, mandatory fresh Verifier). A Verifier FAIL stops the pipeline with the report — do not continue to Stage 2.

When Execute runs one worker per phase, set each worker's model per the **Cost discipline** section (Opus for any phase with a design decision or correctness invariant; Haiku only for a chore dispatched as its own unit; Verifier always Opus), write each brief per **Worker briefs — state the goal, not the steps**, and give each worker scoped gate commands (affected package per commit, full suite at phase boundary — gate targets per the project facts).

## Stage 2 — Publish (herald-finalize)

Invoke `herald-finalize` for branch, commit hygiene, verification notes, and the PR. Include the cycle's planning artifacts (`.specs/features/<cycle>/*`, `.specs/STATE.md`, `.specs/ROADMAP.md` row update) in the PR. Capture the PR number for all later stages.

## Stage 3 — Review (fresh context, author ≠ reviewer)

Spawn ONE subagent via the Agent tool (`general-purpose`, fresh context) with a prompt containing only: the repo, the PR number, and the instruction to invoke the project-local `pr-review` skill for that PR and follow it exactly.

**Do not** pass implementation context, spec content, or this session's reasoning into the subagent — the reviewer's independence is the point of the fresh context (this reproduces a separate `/pr-review` session). Wait for it to finish; its deliverable is comments on the PR, not text returned to you.

## Stage 4 — Triage

1. Fetch every comment: inline `gh api repos/{repo}/pulls/{N}/comments --paginate` and PR-level `gh api repos/{repo}/issues/{N}/comments --paginate`.
2. For each finding, check it against the actual code: is it **real or not**? If real, would you **act on it or not, and why**? Judge on the code as it exists, not on the reviewer's authority; findings that misread the code, duplicate an accepted decision (ADR/AD-NNN), or trade against recorded scope decisions are rejected with the reason.
3. Persist the triage to `.specs/features/<cycle>/review-triage.md` before touching anything: one row per finding — source comment, file:line, verdict (real/false), action (fix/won't-fix), rationale. Comments get deleted in Stage 6, so this file is the only surviving record of the review reasoning.

## Stage 5 — Fix

Apply every "fix" finding. Group into atomic Conventional Commits per `herald-finalize` rules (plain-language messages, no internal IDs, no AI attribution). Re-run the gates before pushing: `make gate-quick` after the fixes land, then the full pre-publish gate (project facts) before the push. Push to the PR branch.

## Stage 6 — Clean Comments

Invoking this skill constitutes the user's standing instruction to delete review comments after triage — the triage record in `review-triage.md` (Stage 4) is the surviving artifact; the comments are scaffolding. Delete ALL comments from the PR:

- inline: each id from `repos/{repo}/pulls/{N}/comments --paginate` via `gh api -X DELETE repos/{repo}/pulls/comments/{id}`
- PR-level: each id from `repos/{repo}/issues/{N}/comments --paginate` via `gh api -X DELETE repos/{repo}/issues/comments/{id}`

Re-fetch both endpoints and verify zero remain. If a submitted *review* (not a comment) exists, it cannot be deleted via the API — report it as a leftover artifact instead of retrying.

## Stage 7 — Merge Gate (the one user prompt)

Present a compact ship report: cycle, PR number, Verifier result, triage counts (real/false, fixed/won't-fix), fix commits, gate results, comment cleanup status. In `once` mode, ask the user (AskUserQuestion): merge now or hold. In `auto`/`until` mode with a clean report, merge without asking.

On approval: `gh pr merge {N} --merge` (merge commit — the repo's convention), then `git checkout main && git pull` and delete the local feature branch.

## Stage 8 — Wrap

Confirm the merged ROADMAP row shows the cycle done (it shipped inside the PR; fix on `main` only if it was missed, as a tiny follow-up). Delete `.specs/.ship-status`. When updating a session-memory topic file (`memory/herald-*-progress.md`), Read it before writing — a Write on an unread file fails.

End the wrap report with, in order: (a) the cycle closed + PR merged; (b) the **next roadmap row** and its scope in one line; (c) a **model recommendation** for that row per Cost discipline, with a one-line rationale — so the user never has to ask "Opus or Fable for the next one?"; (d) a per-subagent output-token table if the data is at hand, noting that `/cost` is the billed authority. Do not start the next cycle automatically — the next run of this skill picks it up.

## Delegation resilience (Stages 1 and 3)

**Idle protocol (bounded):** an idle notification from a worker/reviewer that arrives WITHOUT a completion summary is a stall, not completion. Check observable progress first (Stage 3: inline + issue comment counts via `gh api`; Stage 1: the worker's reported commits/task state). If below expectation, send exactly ONE nudge naming what is missing (e.g. "0 comments posted — continue the review and consolidate"). If a second idle arrives with no new progress, TaskStop the agent and re-dispatch a fresh one with the same brief. Do not babysit beyond this protocol — no ScheduleWakeup loops whose only purpose is re-nudging.

**Limit-death degradation:** a `failed` notification citing a session/usage limit → re-dispatch the agent once. If the re-dispatch also fails, execute that stage's remaining work inline in this session, in the same turn, and record the deviation (e.g. "author = verifier this cycle") in `.specs/STATE.md`. If this session is itself rate-limited, write the heartbeat with exact resume instructions before stopping.

**Outages:** on 2+ consecutive provider 5xx/529 or `gh` connection errors, check the status page once. If a real outage is confirmed: write the heartbeat, schedule ONE long wakeup (15–30 min), and park with a one-line "waiting on <provider>, resuming ~HH:MM" message. Never blocking poll loops, never blind retries.

## Cost discipline — model selection, gates, context (applies across stages)

Default model is **Opus**. Downshift a delegated unit to **Haiku only** — never Sonnet (its per-task output is far larger, erasing the price gap for this pipeline's work). A wrong cheap worker is not free: it costs the bad output *plus* the Verifier catching it *plus* a fix task *plus* re-verification, which can exceed the Opus baseline. So Haiku is a bet on **low slip-probability**, taken only when the task guarantees it.

**Haiku-safe test — downshift a unit only when ALL four hold:**
1. **Fully specified** — exact files, signatures, and steps already in spec/design; no design decision or trade-off left to the worker (it transcribes, it does not invent).
2. **No correctness-critical invariant** — nothing touching idempotency, transaction boundaries, migrations, concurrency, ordering, injection/auth. A weak model fails *quietly* there: passes a thin gate, caught only by the Verifier — or not at all.
3. **A fast local gate catches a slip** — a failing `make gate-quick` / `go vet` / `make lint` surfaces the mistake immediately, so an error is a retry, not a silent defect.
4. **Small blast radius** — few files, no ripple into shared schema/state.

Fail any one → **Opus**. **When unsure → Opus.**

**Per-stage default:**

| Unit | Model |
|---|---|
| Stage 0 preflight · Stage 6 comment cleanup · Stage 8 wrap (pure git/gh/file ops) | Haiku |
| Stage 1 doc-only chores dispatched as their own unit — ADR prose, `.env.example`, settings/deps scaffolding | Haiku |
| Stage 1 phase workers carrying any design decision or correctness invariant (schema/migrations, delivery pipeline, workers, signing, tenancy, idempotency, ordering) | Opus |
| Stage 1 Verifier | Opus — **never downshift**; it is the confidence backbone, and a weak verifier that misses a surviving mutant defeats the pipeline |
| Stage 3 review | governed by `pr-review` — do not override its models |
| Stage 4 triage (real-vs-false against the code) | Opus — adversarial reasoning |
| Stage 5 fixes | Opus by default; Haiku only for a truly local, fully-specified fix that passes the four-condition test |

**Upshifting to Fable 5.** Fable is the strongest model on long-horizon autonomy, on navigating ambiguity, and on reading code for what it *can* do rather than what it currently does — the axis this pipeline is weakest on, since a discrimination sensor can only mutate branches a test already reaches. It costs exactly 2× Opus in both directions ($10/$50 vs $5/$25 per MTok) and takes materially longer per turn, so it is an upshift for specific roles, never a blanket default.

Two preconditions, both hard: the org must allow **30-day data retention** (a zero-retention org gets a 400 on every Fable request, with a valid payload — check this before diagnosing anything else), and Fable's safety classifiers target most cybersecurity content, with its bug-finding gains explicitly **excluding security-focused analysis**.

| Role | Fable 5? |
|---|---|
| Stage 1 Verifier · Stage 4 triage | **Candidate upshift** — short goal-shaped prompts, small token share, and the exact reasoning these roles need |
| `pr-review` Security lane | **Never** — classifier false positives on security-adjacent work, and no bug-finding gain there |
| Stage 1 phase workers | Only after their briefs are goal-stated (below) — a step-listed brief measurably *reduces* Fable's output quality, so it would underperform Opus at double the price |
| Stage 0 / 6 / 8 mechanical ops | Never — Haiku territory |

A ship-cycle **phase is one worker** even when it mixes scaffolding with a hard kernel (e.g. env+config alongside a retry-backoff algorithm or a delivery-ordering edge): the kernel sets the model, so keep the whole phase on Opus. Split a chore to Haiku only when it is dispatched as its own unit. Do **not** treat the Verifier as license to Haiku-ify hard work — its sensor only catches faults it mutates; sensor-blind gaps slip.

## Worker briefs — state the goal, not the steps

A delegated worker's brief must give it **what must be true when it finishes**, and leave *how* to the worker. An ordered recipe caps the worker at what the orchestrator already thought of, which is the wrong ceiling: the expensive defects in this pipeline are the ones nobody enumerated. In past cycles the real bugs lived on branches no test executed — a worker reasoning about what the code can do finds those; a worker transcribing a checklist does not. Over-prescription also measurably degrades Fable 5, so goal-shaped briefs are the prerequisite for upshifting at all.

**Always give (these are context, not prescription):**
- The **seams** — signatures and `file:line` refs from the Explore survey, not file bodies.
- The **binding decisions** — the ADRs and `AD-NNN` rows the phase must conform to, and any accepted assumption it must not relitigate.
- The **invariants that must hold**, named as invariants: "a delivery must never be signed with another tenant's secret", "a retry must not double-deliver within the dedup window". Require a sensor for each; do **not** dictate the test's shape or name.
- **Environment facts** that cost time to rediscover — Go toolchain quirks, env vars the suite needs, the verified baseline counts, services that must be running (Docker for `make gate-full`).
- The **non-negotiable contract** — tests derive from acceptance criteria, gate green before done, one atomic commit per task, no attribution, no internal IDs. This is the definition of done, not a method.
- The **report contract** — what the closing summary must contain, including that deviations be stated plainly rather than buried.

**Don't give:** an ordered list of edits; an enumerated list of tests to write; "cover each of these points" checklists; or a solution the worker is meant to transcribe. If you find yourself writing the implementation into the brief, either the phase is a Haiku chore (where a step list is correct — it transcribes by design) or you are doing the worker's thinking and should hand it the constraint instead.

**Traps are the exception worth naming.** A known landmine — a defaulted field that silently produces the wrong value, a storage method with no guard the caller must supply — belongs in the brief, because it is knowledge the worker cannot derive from the seams. State the trap and the consequence, then require a sensor. That is a constraint, not a step.

**Gate scoping (no quality risk):** run the affected package's tests (`go test -race ./<pkg>/...`) per intermediate task commit; run `make gate-quick` once per phase boundary and the full pre-publish gate once before pushing fixes. The Verifier's discrimination mutations run only the target test file, not the whole suite per mutation.

**Context hygiene (no quality risk):** delegate the opening codebase survey to an `Explore` agent that returns seams — signatures + `file:line` refs, not bodies — instead of reading many full files into the orchestrator context; scope search globs to the Go source tree (`internal/**`, `cmd/**`, `*.go`) and never dump `vendor/`, module caches, or testdata blobs.

## Hygiene (applies to every stage)

- No AI/tooling attribution anywhere public (commits, PR, comments).
- No internal IDs (task/AD/FR/cycle/Gate) in commits, PR bodies, or PR comments — they live only under `.specs/`.
- Multiline `gh` bodies go through `--body-file`/`-F body=@file`, never `-f body=@file`.
- Never post PR-level content as a review (`gh pr review`) — reviews cannot be deleted.
- Wait on CI with `gh pr checks <N> --watch` as a background task, or a Monitor until-loop — never `sleep N && gh …` (the harness blocks it every time).
- Bash cwd resets between calls: use absolute paths, and run git from the repo root. Read any existing file before Edit/Write. Pass both rules into every worker/reviewer brief — subagents hit these errors most.
- Subagents return compact final text (aim well under 10k tokens) — never report files; the orchestrator must be able to Read what comes back.
