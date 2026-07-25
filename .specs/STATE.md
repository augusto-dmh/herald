# STATE — Project Memory

Architecture-level decisions live in `docs/adr/`; entries here are cycle-scoped picks that later cycles must honor. This file references ADRs, never duplicates them.

## Decisions (AD-NNN)

| ID | Decision | Source |
|---|---|---|

## Blockers

- Cycle C (`retry-ladder-and-endpoint-health`) requires drover's reliability core (retry policy, cancel/snooze, rescuer) merged upstream.
- Cycle G (`load-characterization`) requires drover's worker pools merged upstream.

## Known Gaps (non-blocking)

_none_

## Handoff

- **Active feature**: none — founding phase complete (research fleet, ADR-0001..0006, RFC-0001, harness).
- **Branch**: main
- **Next**: cycle A (`walking-skeleton`) via `herald-ship-cycle`.
