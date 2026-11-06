# Architecture

The local React console connects through Vite's same-origin `/v1` proxy to the Go API. The API authenticates each request, derives the source, validates its mutation, and runs a PostgreSQL transaction. No browser cookie authentication, external SaaS, hosted telemetry, or runtime cloud registration is required.

- `backend/internal/profiles`: scalar mutation validation and deterministic ownership policy; no HTTP or database dependencies.
- `backend/internal/platform`: API boundary, development credentials, storage transactions and consistent reads.
- `backend/migrations`: embedded goose SQL migrations, with PostgreSQL advisory locking.
- `contracts`: implemented REST contract and internal outbox payload schemas.
- `web/admin`: read-only operator console; credentials held only in React memory.

Data model: `profiles` holds immutable UUIDs, version and JSONB current attributes. `sources` identifies configured writers. `external_identifiers` enforces source-scoped uniqueness. `attribute_ownership` preserves ownership and tombstones. `mutations` stores request digests and receipts. `provenance` stores value-free attribute decisions. `outbox` holds durable value-free mutation notifications. `goose_db_version` tracks applied schema versions.

The current API supports 100 live attributes and 100 identifiers per profile, 100 attribute operations and 20 identifier additions per request, 64 KiB request bodies, 4 KiB encoded scalar values, and 256-byte identifier values. History is bounded in responses, but storage retention is not automated. Attribute keys are restricted to lowercase ASCII names. Attribute removals can reserve a previously unused name for the removing source; this explicitly records a tombstone.

There is no worker, identity service, consent system, connector or externally published event in this increment. See the initial ADR for exact concurrency and replay semantics.

Application SQL lives in `backend/queries`; sqlc generates typed pgx methods under `internal/platform/db`. The platform store maps these rows to domain models and binds generated queries to native pgx transactions. See [ADR 0002](../adr/0002-sql-first-persistence.md).
