---
description: Read-only status report on the current ship cycle
---

Report the state of the current ship cycle without changing anything. Safe to run from a parallel session while a cycle is in flight.

1. Read `.specs/.ship-status` if it exists; if absent, report that no cycle is in flight and stop.
2. Run `git branch --show-current` and `git status --short` (show at most 10 lines).
3. Run `gh pr list --limit 5`; if a PR is open for the current branch, also `gh pr checks <n>`.
4. End with a single summary line: `<cycle> | <stage> | <branch or PR #> | <one-line health verdict>`.

Do not nudge agents, do not push, do not edit files.
