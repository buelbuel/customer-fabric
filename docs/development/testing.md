# Testing

`make test` starts PostgreSQL, runs Go tests with the race detector, then installs the locked frontend dependencies, type-checks and builds the console. A C toolchain is available in the Go tool image (Alpine with gcc and musl-dev) for the race detector. `go test ./...` without `TEST_DATABASE_URL` skips integration tests; this is not database verification.

Integration tests create a random isolated PostgreSQL schema, apply migrations twice to check repeatability, and drop that schema during cleanup. They never truncate the application schema. The test role needs schema creation permission. Use a disposable development database, never production.

Covered invariants: creation/retrieval/update, invalid payloads, unauthorized access, immutable source attribution, identifier uniqueness and source scoping, optimistic concurrency, sequential and concurrent idempotency, mutation key conflicts, ownership rejection and tombstones, provenance consistency, keyset pagination, and rollback when an injected outbox trigger fails. Accepted state and outbox must commit together. Rejected ownership attempts are audited once and do not emit outbox records.

Unit tests exercise scalar validation, ownership removal checks, fail-closed configuration, duplicate JSON keys, and authorization before storage access. TypeScript checking and a production frontend build verify the console source; they do not replace browser testing.

Dependency review: `bun audit` for frontend packages and `govulncheck ./...` for Go when that tool is available. Container CVE review and production hardening are separate release gates. The CI workflow repeats core checks against PostgreSQL. See verification.md for checks actually executed during the initial implementation.
