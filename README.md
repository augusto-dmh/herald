# herald

Self-hosted webhook delivery for your application's events. Postgres is the only dependency.

Herald sits between your application and your users' endpoints: your app POSTs a message once, herald signs it, delivers it to every subscribed endpoint, retries failures on a documented backoff schedule, disables endpoints that stay broken, and keeps a queryable log of every delivery attempt.

## Why

Sending webhooks looks trivial and isn't. A naive `http.Post` in a request handler loses events on deploy, hammers dead endpoints forever, opens an SSRF hole pointed at your own network, and leaves no trail when a customer asks "why didn't we get the event?". Herald packages the unglamorous parts — signing, retries, endpoint health, delivery history — behind one small service that needs nothing but a Postgres database.

Herald is built on [drover](https://github.com/augusto-dmh/drover), a Postgres-backed task queue by the same author. Message ingestion and delivery jobs commit in the same database transaction, so a message herald has accepted is never silently lost.

## Status

Pre-implementation. The founding research, architecture decisions, and v0.1 roadmap live under [`docs/`](docs/):

- [`docs/research/`](docs/research/) — dated research archive: delivery semantics, signing and replay protection, SSRF-safe egress, the competitive field
- [`docs/adr/`](docs/adr/) — architecture decision records
- [`docs/rfc/`](docs/rfc/) — the v0.1 roadmap

## Design principles

- **Small auditable core** — you can read the whole delivery path in one sitting.
- **Postgres only** — no Redis, no Kafka, no second datastore to operate.
- **At-least-once, honestly** — the delivery contract, retry schedule, and failure policies are documented decisions, not folklore.
- **The reasoning is the product** — every load-bearing choice has an ADR citing dated research.

## License

MIT
