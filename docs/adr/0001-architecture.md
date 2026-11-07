# ADR 0001: Development profile vertical slice

Status: accepted, 2026-11-05. Persistence tooling is refined by [ADR 0002](0002-sql-first-persistence.md).

## Context

The repository contained empty scaffold files. M0 needs real persistence and explainable, secure mutations without implementing later identity, consent, or integration systems.

## Decision

Use one Go module, standard-library HTTP, pgx v5, and PostgreSQL. Keep scalar validation and ownership decisions in `internal/profiles`; HTTP and PostgreSQL adapters live in `internal/platform`. React uses the same REST contract. PostgreSQL stores current state, not an event-sourced reconstruction.

Authentication is explicitly development-only. Startup rejects every mode except `development`. Random bearer credentials are mapped to source names by deployment configuration. All credentials can read all profiles; writes are attributed to the credential's source. This is a trusted local operations interface, not customer self-service or production RBAC.

Attributes are top-level non-null JSON scalars. First accepted writer owns each attribute; only that source can set or remove it. Removal retains a tombstone and ownership. Nested objects, arrays, replacement, transfer, merge, unmerge, event-time ordering, and ownership overrides are unsupported. Explicit fresh writes from the owner can restore a removed value. This is not a general merge engine.

Mutations acquire an advisory lock scoped to source and idempotency key, then lock the profile row. PATCH requires an exact current version. Accepted mutations atomically commit state, ownership, identifiers, metadata-only provenance, an idempotency receipt, and an outbox record. An ownership rejection commits a rejected decision and stable 409 receipt, but no profile change or outbox event. Other failed requests roll back fully and do not reserve the key.

Mutation replay returns the original small receipt, not a historical copy of personal attributes. The hash covers method, path, expected version, and decoded mutation. Object-key ordering is normalized; array ordering remains significant. Idempotency keys are scoped across all mutations for one source. No automatic expiry in M0: expiring receipts without a replay policy would re-enable old mutations.

Identifiers are unique by `(source, namespace, value)`. Addition is explicit in create/PATCH. Reattaching the same identifier to the same profile is safe; another profile conflicts. No email-based matching or reassignment exists.

Outbox records contain only profile/mutation IDs, version, and source. They do not contain attribute values or identifiers. No dispatcher or external delivery exists. Database records are not advertised as CloudEvents; an external transport envelope is deferred.

## Consequences

One transaction boundary makes failure behavior testable. PostgreSQL is the only runtime infrastructure. Automatic migrations are a separate command; startup does not silently migrate. Migration files are embedded and serialized by a database advisory lock.

Listing uses UUID keyset pagination, not chronological ordering or a snapshot across pages. Concurrent inserts below a cursor appear only after restarting pagination. Profile reads use a repeatable-read transaction. Detail shows the most recent 100 attribute decisions. Identifier-only mutations are recorded by mutation receipts and outbox, but do not create attribute provenance.

The initial database role owns its schema for local simplicity. Production must separate migration/application privileges. A bounded global rate limiter is a local abuse guard, not a distributed production control. Explicit retention, erasure, RBAC, TLS and recovery validation remain necessary before real customer data.
