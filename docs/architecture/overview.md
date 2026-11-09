# Architecture

**Status:** M0 and M1 are verified **synthetic-data development** milestones, not a production deployment or GDPR certification. See the [GitHub roadmap](https://github.com/buelbuel/customer-fabric/issues/8) for milestones and release gates. [M0 verification](../development/verification.md) and the [M0 security review](../security/m0-review.md) are dated snapshots; [M1 verification](../development/m1-completion.md) records the merged M1 evidence.

## Runtime components

- `backend/internal/profiles`: scalar mutation validation and deterministic ownership policy; no HTTP or database dependencies.
- `backend/internal/platform`: HTTP boundary, credential mapping, storage transactions and consistent reads. Each request is authenticated first, and source writes derive the source from the credential. An accepted mutation commits state, ownership, identifiers, provenance, receipt and outbox in one native pgx transaction.
- `backend/internal/identity`: Kratos session adapter. Only the verified subject leaves it. Successful checks are never cached.
- `backend/internal/browser`: minimal same-origin HTML reference for Kratos browser flows. Development only.
- `backend/migrations` and `backend/queries`: embedded goose SQL migrations with PostgreSQL advisory locking, and reviewed SQL compiled by sqlc into `internal/platform/db`. See [ADR 0002](../adr/0002-sql-first-persistence.md).
- `contracts`: implemented REST contract and internal outbox payload schemas.
- `web/admin`: read-only operator console.

PostgreSQL is the only application store. There is no worker, connector, consent system, or externally published event.

## Trust boundaries

Authentication, authorization and data provenance are separate. Each audience has its own credential and route family, and a credential from one is rejected on the others.

| Audience | Credential | Permitted surface | Decision record |
| --- | --- | --- | --- |
| Customer | Kratos session: `X-Session-Token`, or the same-origin session cookie on `/v1/customer/profile` | Provision and read the session subject's own profile; redeem a linking grant and list, read or unlink its associations (native sessions only) | [ADR 0005](../adr/0005-customer-profile-linking.md), [ADR 0006](../adr/0006-browser-customer-lifecycle.md) |
| Integration source | Development bearer mapped to a configured source | With customer identity enabled: create profiles and update profiles it created. Without it (M0 stack): also read all profiles | [ADR 0001](../adr/0001-architecture.md), [ADR 0005](../adr/0005-customer-profile-linking.md) |
| Linking issuer | Separate development bearer per source | Issue and revoke grants for that source after an independent proof | [ADR 0007](../adr/0007-verified-enterprise-associations.md) |
| Operator | Separate Kratos issuer with password + TOTP (AAL2) and `profile_reader` role in the operator overlay; a scoped development bearer otherwise | Read profiles on `/v1/operator/profiles` only | [ADR 0004](../adr/0004-authentication-boundaries.md), [ADR 0008](../adr/0008-operator-identity.md) |

Kratos owns customer credentials, email verification, recovery and sessions. Customer Fabric owns profiles. Customer access requires a verified current email. Verified associations relate a subject to one source-record lifetime. They transfer no ownership, merge no records and expose no source attributes. Profiles are never matched by email, name or phone.

## Data model

`profiles` holds immutable UUIDs, version, JSONB current attributes and the creating source. `sources` identifies configured writers. `external_identifiers` enforces source-scoped uniqueness and gives each record binding a lifetime UUID. `attribute_ownership` preserves ownership and tombstones. `mutations` stores request digests and receipts. `provenance` stores value-free attribute decisions. `outbox` holds durable value-free mutation notifications with no consumer. `customer_identity_links` binds a Kratos subject to its profile. `linking_grants` and `verified_associations` record proof-backed relationships. `goose_db_version` tracks applied schema versions.

The API supports 100 live attributes and 100 identifiers per profile, 100 attribute operations and 20 identifier additions per request, 64 KiB request bodies, 4 KiB encoded scalar values, and 256-byte identifier values. History is bounded in responses, but storage retention is not automated. Attribute keys are restricted to lowercase ASCII names. Attribute removals can reserve a previously unused name for the removing source; this explicitly records a tombstone. See [ADR 0001](../adr/0001-architecture.md) for exact concurrency and replay semantics.

## Development-only boundary

The API and browser reference refuse to start unless `CF_MODE=development`. Compose uses loopback HTTP, captured mail, a synthetic proof authority and generated local credentials. Source tokens do not expire, the rate limiter is one process-wide counter, and the database role owns its schema. Purpose-based consent, retention, export, erasure, ingestion, dispatch, segmentation and tested recovery are future milestones. Known image risks and open identity findings are listed in [customer identity](../development/customer-identity.md#readiness).
