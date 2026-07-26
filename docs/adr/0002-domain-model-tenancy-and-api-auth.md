# ADR-0002: Svix-shaped domain model, column-scoped tenancy, and hashed API keys

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: domain-model, tenancy, auth

## Context and Problem Statement

Herald needs an entity set, a multi-tenancy mechanism in Postgres, and an authentication scheme for a machine-first API — chosen once, because every table, index, and endpoint hangs off them.

## Decision Drivers

- The Svix vocabulary (application/endpoint/message/attempt) is what receivers and integrators already understand.
- Tenancy must be safe by construction in every query without imposing v0.1-irrelevant operational cost.
- Delivery-log queries are keyset-paginated dashboards; ID choice decides index locality.
- API consumers are machines; the auth scheme must be verifiable in one indexed lookup.

## Considered Options

**Tenant → application → endpoint with `tenant_id` columns** · **Flat tenant → endpoint (no application layer)** · **Schema-per-tenant** · **Postgres RLS from day one**

## Decision Outcome

Chosen: **Tenant → application → endpoint with `tenant_id` columns**, with:

- **Entities**: tenants, api_keys, applications (`uid` addressing), endpoints (URL, `filter_types[]`, disabled state), endpoint_secrets, messages, deliveries (one per message × endpoint, the API-facing projection), delivery_attempts. No event-type registry table in v0.1 — `filter_types` matches on message `event_type` strings.
- **Tenancy**: `tenant_id` on every tenant-owned row plus composite indexes leading with it; schema is RLS-ready but RLS is deferred.
- **IDs**: UUIDv7 everywhere — time-ordered for B-tree locality and natural keyset-pagination cursors.
- **Auth**: prefixed keys (`hrld_live_…`), SHA-256 hashed at rest, shown once at creation, throttled `last_used_at`, `full` and `ingest` scopes on a single API surface.

### Positive Consequences

- One mental model shared with the ecosystem's reference product; integrators need no glossary.
- Column tenancy + composite indexes is the cheapest correct mechanism at v0.1 scale, with an RLS upgrade path that needs no schema change.

### Negative Consequences

- Application layer adds one join to every endpoint-scoped query even for single-app tenants.
- String-matched event types defer validation a registry would give; typos in `filter_types` fail silently.

## Links

- Evidence: `docs/research/2026-07-25/rq05-domain-model-and-drover-seam.md`
- Related: ADR-0005
