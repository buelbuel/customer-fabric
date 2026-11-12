# Architecture

**Status:** M0 and M1 are verified **synthetic-data development** milestones, not a production deployment or GDPR certification. See the [GitHub roadmap](https://github.com/buelbuel/customer-fabric/issues/8) for milestones and release gates. The [roadmap summary](roadmap.md) links each milestone's verification evidence.

## Runtime components

- `backend/internal/profiles`: scalar mutation validation and deterministic ownership policy; no HTTP or database dependencies.
- `backend/internal/platform`: HTTP boundary, credential mapping, storage transactions and consistent reads. Each request is authenticated first, and source writes derive the source from the credential. An accepted mutation commits state, ownership, identifiers, provenance, receipt and outbox in one native pgx transaction.
- `backend/internal/identity`: Kratos session adapter. Only the verified subject leaves it. Successful checks are never cached.
- `backend/internal/browser`: minimal same-origin HTML reference for Kratos browser flows. Development only.
- `backend/migrations` and `backend/queries`: embedded goose SQL migrations with PostgreSQL advisory locking, and reviewed SQL compiled by sqlc into `internal/platform/db`. See [ADR 0002](../adr/0002-sql-first-persistence.md).
- `contracts`: implemented REST contract and internal outbox payload schemas.
- `web/admin`: read-only operator console.

PostgreSQL is the only application store. There is no worker, connector, or externally published event.

## Trust boundaries

Authentication, authorization and data provenance are separate. Each audience has its own credential and route family, and a credential from one is rejected on the others.

| Audience | Credential | Permitted surface | Decision record |
| --- | --- | --- | --- |
| Customer | Kratos session: exactly one of `X-Session-Token` or the configured same-origin session cookie, on every customer route. Cookie writes also need the exact Origin | Provision and read the session subject's own profile; redeem a linking grant and list, read or unlink its associations; view, grant and withdraw their own consent | [ADR 0005](../adr/0005-customer-profile-linking.md), [ADR 0006](../adr/0006-browser-customer-lifecycle.md), [ADR 0011](../adr/0011-customer-consent-api.md) |
| Integration source | Development bearer mapped to a configured source | With customer identity enabled: create profiles, update profiles it created and send events for its own records (`POST /v1/source-events`). Without it (M0 stack): also read all profiles | [ADR 0001](../adr/0001-architecture.md), [ADR 0005](../adr/0005-customer-profile-linking.md) |
| Linking issuer | Separate development bearer per source | Issue and revoke grants for that source after an independent proof | [ADR 0007](../adr/0007-verified-enterprise-associations.md) |
| Operator | Separate Kratos issuer with password + TOTP (AAL2) and named roles (`profile_reader` for reads) | Read profiles on `/v1/operator/profiles`; read a customer's unified view of verified source records on `/v1/operator/customers/{id}/unified-view`; read consent policies and effective permission under `/v1/operator/consent/`; read attribute definitions and access policies under `/v1/operator/attributes`. Each write needs the named role for its area, such as `governance_admin` for attribute definitions and access policies, `consent_admin` for consent policies and `privacy_admin` for external processing | [ADR 0004](../adr/0004-authentication-boundaries.md), [ADR 0008](../adr/0008-operator-identity.md) |

Kratos owns customer credentials, email verification, recovery and sessions. Customer Fabric owns profiles. Customer access requires a verified current email. Verified associations relate a subject to one source-record lifetime. They transfer no ownership, merge no records and expose no source attributes to customers. Profiles are never matched by email, name or phone.

The operator unified view reads a customer's active verified records in one read-only snapshot and stores nothing. It selects no winner: each value is listed with its source record and write, and disagreement is reported as a conflict. A value appears only if its attribute is defined, the attribute's current access policy names the reader (`operator` here; `segmentation` and `activation` are the other readers) and the record's source, the record's source owns it, and an accepted write by that source exists. Access is denied by default: an undefined attribute cannot be given a policy, and an attribute without one is withheld. A policy change applies to the next read and never changes ownership or provenance. Customers are not a reader. A record with several identifiers, one its source did not create, or one that is restricted or whose record lifecycle is not active contributes no values, and a customer whose own lifecycle is not active has no view.

## Data model

`profiles` holds immutable UUIDs, version, JSONB current attributes and the creating source. `sources` identifies configured writers. `external_identifiers` enforces source-scoped uniqueness and gives each record binding a lifetime UUID. `attribute_ownership` preserves ownership and tombstones. `mutations` stores request digests and receipts. `provenance` stores value-free attribute decisions. `outbox` holds durable value-free mutation notifications with no consumer. `customer_identity_links` binds a Kratos subject to its profile. `linking_grants` and `verified_associations` record proof-backed relationships. `attribute_definitions` holds immutable, widening-only type revisions per attribute name; `provenance` records the revision each accepted typed value was validated under. `attribute_access_policies` holds append-only revisions of which readers may see a defined attribute and from which owning sources. `goose_db_version` tracks applied schema versions.

The API supports 100 live attributes and 100 identifiers per profile, 100 attribute operations and 20 identifier additions per request, 64 KiB request bodies, 4 KiB encoded scalar values, and 256-byte identifier values. History is bounded in responses, but storage retention is not automated. Attribute keys are restricted to lowercase ASCII names. A defined attribute accepts only values of its type (string, integer, decimal, boolean, date, timestamp or enum) on every source write; an undefined one stays an untyped scalar. Attribute removals can reserve a previously unused name for the removing source; this explicitly records a tombstone. See [ADR 0001](../adr/0001-architecture.md) for exact concurrency and replay semantics.

Two limits sit outside that ADR. The quoted version ETag is an optimistic mutation token: rejected provenance can change the enriched detail response without changing the version, so it must not be treated as a byte-for-byte cache validator. Responses are `no-store` and conditional GET is unsupported; enabling HTTP caching first requires a separate representation validator. Multi-identifier claims can deadlock when two requests take them in opposing order. PostgreSQL aborts one transaction safely and the caller may retry with the same key, so no claim of deadlock-free execution is made.

## Development-only boundary

The API and browser reference refuse to start unless `CF_MODE=development`. Compose uses loopback HTTP, captured mail, a synthetic proof authority and generated local credentials. Source tokens do not expire, the rate limiter is one process-wide counter, and the database role owns its schema. Retention, export, erasure, ingestion, dispatch, segmentation and tested recovery are future milestones. Known image risks and open identity findings are listed in the [security guide](../security/eu-first.md#open-findings-and-production-blockers).

## Invariants for later milestones

M1 established these guarantees, and M2 builds on them.

- One verified Kratos subject resolves to one stable Customer Fabric profile. Email changes, re-verification and password recovery preserve the subject link. A new subject does not inherit a profile or relationship by email.
- A verified association has its own ID, source and external record lifetime. A matching attribute conveys no authorization. Source data and attribute ownership remain separate, and customer-facing linkage exposes association metadata only.
- One active association can reference a given source-record lifetime. Revocation and source record deletion suppress access. Recreating the identifier does not restore the old relationship. The trusted issuer must report loss of upstream authority.
- Operator and source credentials use separate trust and permission checks. Customer sessions confer access to their own resources only. Source rotation preserves stable source identity, ownership and replay scope.

M2 should use explicit customer, profile, source-record and association references when modeling consent purpose, legal basis, evidence, versioning, withdrawal and downstream propagation. Consent changes cannot be inferred from profile linking or mailbox verification. Erasure needs coordinated fences for Kratos identities and sessions, profile links, outstanding grants and associations. It must minimize association audit history and handle provenance, outbox, idempotency receipts and backups under [ADR 0003](../adr/0003-data-lifecycle.md). Backup restore must reapply revocation and erasure fences before reopening access.
