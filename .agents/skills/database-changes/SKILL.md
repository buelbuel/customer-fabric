---
name: database-changes
description: Change Customer Fabric PostgreSQL migrations, sqlc queries, constraints, or transaction behavior.
---

1. Read [ADR 0002](../../../docs/adr/0002-sql-first-persistence.md) and the [database guide](../../../docs/development/local.md#database-changes).
2. Add the next numbered goose migration for schema changes, written as the target schema: no compatibility columns, dual reads or data-migration paths for unreleased formats. If existing local rows cannot be kept, make the migration refuse with a clear message rather than drop them. Edit named queries under `backend/queries`; bind generated queries with `WithTx(tx)` when writing profile, provenance and outbox together. Keep domain decisions out of the queries.
3. Run `make generate` and review the SQL and generated diff together. Commit both. Use focused regression tests for constraints, migration behavior or rollback as applicable.
4. Run `make check-generated` and `make test` against the isolated Compose PostgreSQL. Report skips explicitly. Use `make migrate` only for reviewed Up migrations on local development data.
