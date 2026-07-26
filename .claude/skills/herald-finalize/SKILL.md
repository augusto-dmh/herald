---
name: herald-finalize
description: Finalizes and publishes Herald changes with consistent Git branch names, Conventional Commit messages, verification notes, and structured pull requests. Use when the user asks to finalize work, prepare a commit, commit changes, push a branch, open a pull request, write a PR description, or publish completed Herald work. Do not use for implementing features, reviewing code, or debugging CI unless the user also asks to publish the resulting changes.
license: CC-BY-4.0
metadata:
  author: Herald contributors
  version: 1.0.0
---

# Herald Finalize

## Project facts

Porting this skill to another project means editing only this section.

- **Repo:** `augusto-dmh/herald` — a self-hosted multi-tenant webhook delivery service, built on the drover task-queue library. Default branch: `main`.
- **Language:** Go. Module path: `github.com/augusto-dmh/herald`.
- **Gates:** the root `Makefile` is the single source of truth for gate commands — this skill references target names only. `make gate-quick` (unit tests with the race detector) · `make gate-full` (gate-quick plus integration tests, which use testcontainers — probe `docker ps` first and tell the user to start Docker Desktop rather than debugging further) · `make build` (compile + vet) · `make lint` (pinned golangci-lint). Pre-publish gate: `make build && make gate-full && make lint`.
- **Docs:** `docs/adr/` (accepted decisions), `docs/rfc/` (proposals; the roadmap RFC — RFC-0001 for v0.1 — defines cycle scope), `docs/research/YYYY-MM-DD/` (durable research).
- **Working state:** `.specs/STATE.md`, `.specs/ROADMAP.md`, `.specs/features/<cycle>/`, heartbeat `.specs/.ship-status` (gitignored).

## Scope

Apply the repository conventions above when preparing or publishing completed work. Treat a request to finalize completed work as authorization to inspect the diff, choose metadata, stage intended files, commit, push, and create an open ready-for-review PR. Keep narrower requests proportional: generate names and text when that is all the user requests, and stop after committing when the user asks only for a commit.

## Conventions

Use Conventional Commits for commit messages and PR titles:

```text
<type>(<optional-scope>)<optional-!>: <imperative summary>
```

Use one of these types: `feat`, `fix`, `docs`, `refactor`, `test`, `chore`, `build`, `ci`, `perf`, `style`, `revert`.

Use branch names in this format:

```text
<type>/<optional-issue-number-><short-kebab-summary>
```

Keep the branch type aligned with the primary change. Keep summaries concise, specific, and lowercase. Examples:

```text
docs/stack-decision
chore/workflow-conventions
feat/endpoint-registration
```

PR titles follow the same Conventional Commit format as commits and summarize the whole PR.

## Commit And PR Hygiene

Never add authorship or tooling attribution to commits or pull requests. Commit messages and PR bodies must not contain `Co-Authored-By` trailers, "Generated with" lines, model names, or any other identification of an AI assistant or the tool used to produce the change.

Write PR-body and issue paragraphs as single unwrapped lines. Never hard-wrap prose at a column width — GitHub renders the wraps literally, squeezing the text into a narrow left column.

## Self-Contained History

Commit messages, PR titles, and PR bodies must be understandable by an outside reader with no access to Herald's internal planning. They must NOT contain internal references:

- task or phase IDs from `tlc-spec-driven` (`A1`, `B2`, `T3`, `Phase 3`);
- decision or requirement IDs (`ADR-0004`, `AD-006`, `RFC-0001`, `FR-DELIVERY-009`, `NFR-SEC-002`, `AC-2`);
- `Gap-N`, `cycle N`, `cycle A`, `Gate:` labels, `SPEC_DEVIATION`, `design §N`;
- paths into internal working state (`.specs/…`).

Explain each change in plain terms instead. Internal traceability lives in `.specs/` (STATE.md, tasks.md), never in the permanent git history or on the PR. Name a document (for example an ADR) only when the change actually adds or edits that file — not as a cross-reference the reader must look up. The register is: a short Summary, bulleted Changes, bulleted Verification, no lookups.

`validate_metadata.py` fails on these tokens in a commit subject/body or PR title; `render_pr_body.py` warns when the assembled body contains them. Clear both before publishing, and do not mirror the internal-reference style of surrounding commits — that style is the problem being corrected.

## Workflow

### Step 1: Inspect The Repository

1. Run `git status --short`, `git branch --show-current`, and `git diff --stat`.
2. Read the relevant diff before proposing metadata.
3. Identify unrelated working-tree changes and leave them unstaged.
4. If the task is only to suggest names or draft a PR description, stop before mutating Git state.

### Step 2: Choose The Metadata

1. Select the primary Conventional Commit type.
2. Add a scope only when it improves clarity, such as `workflow`, `stack`, `research`, `docs`, or a future domain name.
3. Write an imperative summary that describes the outcome, not implementation mechanics.
4. Derive the branch name from the same primary change.
5. Run:

```bash
python3 .claude/skills/herald-finalize/scripts/validate_metadata.py \
  --branch '<branch-name>' \
  --commit '<commit-message>' \
  --pr-title '<pr-title>'
```

Fix validation errors before continuing.

### Step 3: Verify The Change

1. Run the narrowest relevant checks for the changed files.
2. When the change touches Go code, run `make build` and `make gate-quick`; when storage, migration, delivery, or end-to-end behavior changed, also run `make gate-full` (probe `docker ps` first — see the project facts).
3. For docs-only or skill-only changes, run `git diff --check` and any relevant helper script validation.
4. Before publishing app code, run the full pre-publish gate from the project facts: `make build && make gate-full && make lint` — CI enforces lint, so an unlinted commit fails the pipeline.
5. Report any verification that could not run.
6. Do not publish changes with known failing verification unless the user explicitly accepts that risk.

### Step 4: Commit Intentionally

1. Stage only files that belong to the requested change.
2. Prefer one atomic commit per logical concern.
3. When changes span unrelated concerns, present the proposed commit breakdown and wait for user approval before committing.
4. Review `git diff --cached --stat` and `git diff --cached` before each commit.
5. Validate the commit message with the metadata validator.
6. Run `git status --short` after committing and report remaining unstaged or untracked files.

### Step 5: Push And Open The PR

1. Run `gh auth status` before publishing. If authentication is unavailable, report the blocker.
2. Push the branch with an upstream automatically when the user asks to finalize or publish completed work.
3. Draft concise Markdown for Summary, Changes, and Verification from the diff and verification output.
4. Run `scripts/render_pr_body.py` to assemble the PR body. Pass screenshots only when the PR contains visible UI changes. Pass related issues only when applicable.

```bash
python3 .claude/skills/herald-finalize/scripts/render_pr_body.py \
  --summary-file /tmp/herald-pr-summary.md \
  --changes-file /tmp/herald-pr-changes.md \
  --verification-file /tmp/herald-pr-verification.md \
  --output /tmp/herald-pr-body.md
```

5. Read [assets/pull_request_template.md](assets/pull_request_template.md) only when the renderer cannot be used or the user explicitly asks to inspect the template.
6. Create an open ready-for-review PR with `gh pr create --base <base-branch> --head <branch-name> --title '<pr-title>' --body-file <pr-body-file>`. Do not create draft PRs unless the user asks for one.
7. When a PR already exists, update its title or body with `gh pr edit`.

## Examples

### Workflow Change

User says: "Finalize the workflow convention docs."

Use:

```text
Branch: chore/workflow-conventions
Commit: chore(workflow): document project workflow conventions
PR title: chore(workflow): document project workflow conventions
```

### Research Or Decision Change

User says: "Commit the retry-policy ADR."

Use:

```text
Branch: docs/retry-policy-decision
Commit: docs(delivery): record webhook retry and backoff policy decision
PR title: docs(delivery): record webhook retry and backoff policy decision
```

### Naming Only

User says: "Suggest a branch name and commit message for the signing RFC."

Return:

```text
Branch: docs/signing-rfc
Commit: docs(signing): propose webhook payload signature scheme
```

Do not modify Git state.

## Troubleshooting

### Validation Rejects The Metadata

Keep the type lowercase, use kebab-case after the branch slash, and format commits and PR titles as `<type>(<optional-scope>): <summary>`.

### The Working Tree Contains Unrelated Changes

Stage only the files that belong to the requested change. Report the remaining files without reverting or including them.

### GitHub PR Creation Is Unavailable

Run `gh auth status` and report the authentication or repository-access blocker. Return the validated PR title and populated Markdown body.

### The PR Body Renderer Cannot Run

Read [assets/pull_request_template.md](assets/pull_request_template.md), populate the applicable sections manually, and remove optional sections that do not apply.
