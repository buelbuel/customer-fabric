# ADR 0002: SQL-first persistence with sqlc and goose

Status: accepted, 2026-11-06. Refines ADR 0001 without changing the profile API or ownership policy.

## Decision

PostgreSQL remains authoritative. pgx/v5 and pgxpool remain the application driver and pool. Reviewed SQL in `backend/queries` is compiled by pinned sqlc 1.31.1 into `internal/platform/db`. Generated code is committed; CI runs `sqlc diff` to detect missing regeneration. The schema input is the goose migration directory, so there is no separately maintained schema snapshot.

The domain package remains independent of generated models, pgx and HTTP. The platform adapter maps generated rows into domain values. It opens native pgx transactions, binds the generated queries with `WithTx`, and explicitly commits or rolls back. Profile state, ownership, identifiers, mutation receipts, provenance and outbox stay within the same transaction. Consistent detail reads retain repeatable-read transactions.

Goose 3.29.0 manages numbered SQL migrations. Each Up/Down is explicit and reviewable. Embedded migrations are discovered automatically. Goose requires database/sql, so the migration entry point uses pgx's `stdlib.OpenDBFromPool`; database/sql is confined to migration tooling. Application persistence stays native pgx. There is no ORM or custom query framework.

Use a PostgreSQL session advisory lock to serialize goose runners. Goose alone tracks versions in its built-in store. The foundation migration creates the complete current schema on a fresh database, without conditional table creation or schema adoption.

## Pre-release baseline

Customer Fabric is unreleased; obsolete development schemas have no supported upgrade path. The compatibility adapter and its tests were removed on 2026-11-06. Unexpected existing table shapes fail explicitly; normal migration execution never drops them or resets a database. Existing databases already on the current goose baseline need no reset. Historical decisions remain available in Git, not application compatibility code.

## Consequences

SQL remains the source of truth, with compiler-checked generated query signatures and real PostgreSQL tests for behavior. Dependency versions and generated output are reproducible. Goose requires Go 1.26 or newer; development/CI images already use Go 1.27.3. Goose and sqlc are MIT licensed. sqlc runs only as a build tool.

Down is available in SQL for review and disposable-database tests, but the application's migration command applies Up only. Production schema recovery should use reviewed roll-forward migrations and tested backups. Goose does not automatically enforce immutable migration checksums; code review must reject edits to already-applied SQL.
