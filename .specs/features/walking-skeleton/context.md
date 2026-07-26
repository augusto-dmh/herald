# Cycle walking-skeleton — Decision Log

## D-1 (AD-001): Query layer — raw pgx over sqlc

Options: sqlc (drover's choice; generated type safety, needs generate step + drift CI job) · raw pgx queries in a store package (zero toolchain, hand-audited SQL). Chosen: raw pgx. An application's query set at skeleton stage is ~15 statements; sqlc's payoff arrives with scale, its cost (generator pin, drift job, generated-code review noise) arrives immediately. Revisit at ~30+ queries or first refactor pain.

## D-2 (AD-002): Herald owns its migration runner

Options: goose/tern third-party migrator · drover-pattern embed.FS runner with `herald_migrations` version table. Chosen: drover-pattern runner. Proven in-house pattern, no new dependency, auditable; drover's own schema applies via the exported `drover.Migrate`.

## D-3 (AD-003): Tenant creation is operator-bootstrapped

Options: unauthenticated signup (a product decision this cycle must not make) · seed-only via SQL (not e2e-testable through the API) · `POST /v1/tenants` guarded by `HERALD_BOOTSTRAP_TOKEN`. Chosen: bootstrap token. Minimal, testable, defers the signup product question without blocking the e2e slice.

## D-4 (AD-004): stdlib `net/http` routing, no framework

Options: chi/echo (conveniences, dependency) · stdlib Go 1.22 method+pattern ServeMux. Chosen: stdlib. The route table is five entries; a framework buys nothing but a dependency against the auditable-core identity.

## D-5 (AD-005): Job args carry only the delivery ID

Options: full payload snapshot in job args (fewer reads, stale-data risk) · delivery ID only, worker re-reads state. Chosen: ID only. ADR-0005's projection rule plus freshness: endpoint URL or disabled-state changes between enqueue and execution are honored automatically.

## D-6 (AD-006): Drover pinned to `@main` pseudo-version

Options: wait for a drover tag (blocks the cycle) · pin main's commit. Chosen: pin. Drover cycle A is merged and sufficient; the pseudo-version pins an exact commit, upgraded deliberately at drover v0.1.0.

## D-7 (AD-007): UUIDv7 via `github.com/google/uuid`

Options: gofrs/uuid · google/uuid `NewV7`. Chosen: google/uuid — maintained, ubiquitous, satisfies ADR-0002's time-ordered requirement.
