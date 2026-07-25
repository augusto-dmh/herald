# ADR-0001: Herald is an API-first, Postgres-only webhook delivery service

- **Date**: 2026-07-25
- **Status**: Accepted (2026-07-25)
- **Deciders**: Augusto de Melo Henriques
- **Tags**: scope, architecture

## Context and Problem Statement

Every credible product in this space (Svix, Convoy, Outpost) requires at least Postgres plus Redis, and often multiple service roles, to self-host; meanwhile "just send it from the request handler" loses events, retries nothing, and leaves no audit trail. Herald must pick the scope that a solo-maintained, small-core service can deliver credibly.

## Decision Drivers

- Self-hosting complexity is the field's weakest point: no competitor runs on a single datastore.
- Drover provides transactional enqueue in Postgres, eliminating the dual-write window that Redis/MQ-queued competitors accept.
- A small auditable core is the project's identity; every feature must earn its operational surface.
- The maintainer ships PR-sized cycles; scope walls must be explicit and durable.

## Considered Options

**API-first sender, Postgres-only, single binary** · **Svix-workalike including consumer portal UI** · **General event-destination router (Outpost-style)**

## Decision Outcome

Chosen: **API-first sender, Postgres-only, single binary**, with:

- **In scope (v0.1)**: message ingestion API, Standard Webhooks signing, retry ladder, per-attempt queryable delivery logs, automatic endpoint disabling with manual re-enable, single-message redelivery, per-endpoint politeness, SSRF-safe egress.
- **Walls (out for the life of v0.1, revisited only by RFC)**: consumer portal UI, payload transformations, non-HTTP destinations (queues, functions), static egress IPs, FIFO/ordered delivery guarantees.
- **Positioning**: the webhook sender you can operate with nothing but the Postgres you already run.

### Positive Consequences

- Occupies a real gap: no competitor offers Postgres-only with transactional enqueue.
- Every wall removes an entire operational subsystem (UI stack, transformation runtime, per-destination adapters).

### Negative Consequences

- No portal UI means tenants' consumers are served only through the tenant's own tooling.
- Refusing FIFO forever closes the door on use cases that need ordered delivery.

## Links

- Evidence: `docs/research/2026-07-25/rq03-competitive-landscape-and-scope.md`
- Related: ADR-0005
