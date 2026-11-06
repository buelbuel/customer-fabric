# Database development

Use PostgreSQL, pgx/v5, pgxpool, sqlc and goose. Domain rules stay in `internal/profiles`; SQL belongs in `backend/queries`. Generated code in `backend/internal/platform/db` is an adapter detail, never a domain dependency.

## Change a query

1. Edit a named SQL query in `backend/queries/profiles.sql` (or add a focused query file).
2. Run `make generate` using pinned sqlc 1.31.1.
3. Update the platform adapter to use the generated parameter/result types.
4. Run `make test` against real PostgreSQL and commit SQL plus generated code together.

`make check-generated` runs `sqlc diff`, which fails when generated output is stale. CI repeats this check. Hand-written copies of generated methods are not accepted. String UUID and time.Time overrides keep conversion at the persistence boundary small; database constraints still enforce UUID validity. JSONB remains raw JSON at the query boundary and is decoded by the adapter.

## Change the schema

Add the next numbered migration, for example `backend/migrations/002_profile_example.sql`, with explicit `-- +goose Up` and `-- +goose Down` sections. Follow goose's `StatementBegin`/`StatementEnd` syntax for procedural SQL. Do not modify applied migrations or use runtime schema auto-sync. sqlc reads these same migration files and ignores Down sections.

Run `make generate`, review both SQL and generated changes, and add tests for the new constraints or behavior. `make test` creates isolated PostgreSQL schemas and exercises real migrations. `make migrate` builds and runs the application migration command against the local database. It applies only pending Up migrations; existing migrations are not replayed.

Goose runs migrations in transactions by default. Do not add `NO TRANSACTION` without a documented requirement and a failure/recovery plan. Native application pgx transactions must wrap all related profile, provenance and outbox writes, with generated queries bound using `WithTx(tx)`.

## Clean pre-release baseline

`make migrate` initializes new databases with goose alone. Obsolete development schemas are unsupported and fail if tables conflict with the baseline. No migration command resets data or Docker volumes. Databases already using the current goose baseline require no reset. Manual schema drift is never hidden with `CREATE TABLE IF NOT EXISTS`.

The initial Down migration deletes all profile tables. It is exercised only in isolated test schemas and is not exposed as an application command. Do not use it against retained data.

See [ADR 0002](../adr/0002-sql-first-persistence.md) for locking and runtime boundaries. Official references: [sqlc pgx support](https://docs.sqlc.dev/en/latest/guides/using-go-and-pgx.html), [goose provider](https://pressly.github.io/goose/documentation/provider/).
