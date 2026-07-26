# STATE — Project Memory

Architecture-level decisions live in `docs/adr/`; entries here are cycle-scoped picks that later cycles must honor. This file references ADRs, never duplicates them.

## Decisions (AD-NNN)

| ID | Decision | Source |
|---|---|---|
| AD-001 | Query layer: hand-written pgx, no sqlc; revisit at ~30+ queries | walking-skeleton context.md D-1 |
| AD-002 | Herald-owned embed.FS migration runner + `herald_migrations` table; drover schema via `drover.Migrate` | walking-skeleton context.md D-2 |
| AD-003 | Tenant creation via `POST /v1/tenants` guarded by `HERALD_BOOTSTRAP_TOKEN` | walking-skeleton context.md D-3 |
| AD-004 | stdlib `net/http` pattern routing, no framework | walking-skeleton context.md D-4 |
| AD-005 | Delivery job args carry only the delivery ID; worker re-reads state | walking-skeleton context.md D-5 |
| AD-006 | drover pinned `@main` pseudo-version until drover tags v0.1.0 | walking-skeleton context.md D-6 |
| AD-007 | UUIDv7 via `github.com/google/uuid` | walking-skeleton context.md D-7 |
| AD-008 | User-directed: foundation commits ride the cycle-A PR branch; `main` holds only the init commit until that PR merges (single PR, manual merge approval) | this session |

## Blockers

- Cycle C (`retry-ladder-and-endpoint-health`) requires drover's reliability core (retry policy, cancel/snooze, rescuer) merged upstream.
- Cycle G (`load-characterization`) requires drover's worker pools merged upstream.

## Known Gaps (non-blocking)

_none_

## Handoff

- **Active feature**: none — `walking-skeleton` shipped in PR #1 (foundation docs + harness + the full cycle: 11 code commits, 77 tests — 31 unit / 46 integration, Verifier PASS with 0 surviving mutants across 8 mutations, all gates green). The PR also carries the founding research, ADR-0001..0006, RFC-0001, and the agent harness per AD-008.
- **Branch**: main
- **Next**: cycle B (`signing-and-secrets`) via `herald-ship-cycle`.
