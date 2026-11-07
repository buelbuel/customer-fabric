# Working on Customer Fabric

Read README and docs/adr/0001-architecture.md before changing mutation semantics. Use a single Go module in backend. Keep domain rules independent of HTTP and database drivers. Preserve transactional invariants and parameterized SQL. Never log tokens, payloads or customer values.

Keep changes small and working. Run `make test` with real PostgreSQL; explicitly report skipped tests. Update OpenAPI and tests when changing endpoints. Do not add speculative domains, services or production authentication shortcuts. Development mode must fail closed in every other mode. Use synthetic data only. No SDKs or visual builders.

Do not overwrite local .env files or delete database volumes during normal startup. Apache-2.0 original code; DCO sign-off required for contributions. Major decisions belong in docs/adr.

Database persistence is SQL-first: pgx/v5 and pgxpool, sqlc-generated pgx queries, and goose SQL migrations. Edit backend/queries and versioned migrations, run `make generate`, and commit generated code. Never hand-edit generated files. `make check-generated` and CI must pass. Keep domain rules outside generated queries; bind queries to native pgx transactions for profile, provenance and outbox writes. No ORM or custom SQL framework. Use focused Git commits and keep unrelated changes separate.
