# Customer Fabric architecture

**Status:** M0 and M1 are verified **synthetic-data development** milestones. This is not a production deployment or GDPR certification. For milestones and release gates, see [the GitHub roadmap](https://github.com/buelbuel/customer-fabric/issues/8).

## Runtime components

- **Go modular monolith:** standard-library HTTP in `backend/internal/platform`; domain validation and source attribute ownership in `backend/internal/profiles`. APIs distinguish customer, operator, integration-source and linking-issuer trust boundaries.
- **PostgreSQL:** profile current-state JSONB, external identifiers, immutable creator-source authority, ownership tombstones, source attribution, provenance, mutation receipts and metadata-only transactional outbox. Explicit Goose migrations and reviewed SQL generated with sqlc/pgx. `backend/internal/platform` binds queries to native pgx transactions.
- **Customer identity:** Ory Kratos handles registration, verified email, login, recovery, settings, session and logout. Customer Fabric checks live Kratos sessions before allowing access to the subject-linked stable profile. Browser reference forms use native Kratos browser flows, cookies and CSRF; native API clients use session tokens.
- **Operator identity:** the optional M1 operator overlay runs a separate Kratos issuer, database and secrets, with named accounts, TOTP/AAL2 and explicit read role. The older isolated compatibility development stack can still use a scoped development operator bearer. Neither should be confused with a production authorization system.
- **Source identities:** development integration credentials identify trusted source writers. Source access in M1 permits creation and modification of profiles created by that source; it does not allow enumerating other profiles. Creator authority is stored on profiles, **not inferred from outbox history**.
- **Verified external associations:** dedicated linking issuer credentials attest a successful independent synthetic proof and issue short-lived, single-use grants. A verified Kratos customer can redeem a grant for an external record. The association is not an attribute merge, data-ownership transfer or permission to read source-controlled attributes.
- **React/Bun:** read-only operator console and separate minimal semantic HTML customer reference. Docker Compose uses loopback-only published ports and isolated profiles/volumes in worktrees.

## Persistence and invariants

An accepted source mutation atomically writes versioned state, field ownership, identifiers, provenance, an idempotency receipt and a metadata-only outbox record. Ownership rejection records a stable denied decision without changing the profile. Optimistic concurrency uses an exact current version; same-source idempotency keys replay their original receipt.

External identifier uniqueness is `(source, namespace, value)`; the record lifetime has a separate UUID used for verified association fencing. Profiles are **not automatically matched or merged by email**, names or phone numbers. Each Kratos subject may link to its own stable customer profile, while verified external associations are represented separately. Expiry, revocation and deletion of source records suppress invalid links, without rewriting historical source data.

Profile attributes currently support top-level, non-null scalar values only, with bounded operation sizes. An association alone exposes metadata, not source profile values. SQL remains reviewed and parameterized; the domain layer does not depend on generated DB types.

## Deployment boundaries and remaining work

The Compose configuration is **development-only**: local HTTP, captured email, synthetic proof authority and local/generated credentials. No real customer data may be processed. CI validates Go race/integration and migrations, sqlc, browser Kratos flows, API contracts, frontend and image scans, but it is not a security audit.

**Not yet implemented:** purpose-based consent, retention/erasure/export, outbox dispatcher and connectors, record ingestion/event ordering, segmentation/activation, production TLS/secrets, least-privilege database roles, tested backup recovery and production service credential lifecycle. The Kratos image still has documented critical/high scan findings, reported separately rather than suppressed.

## Design references

- [Profile semantics (ADR 0001)](../adr/0001-architecture.md), [SQL persistence (ADR 0002)](../adr/0002-sql-first-persistence.md), [Lifecycle design (ADR 0003)](../adr/0003-data-lifecycle.md).
- [Authentication boundaries (ADR 0004)](../adr/0004-authentication-boundaries.md), [Kratos subject/profile linking (ADR 0005)](../adr/0005-customer-profile-linking.md), [Browser flows (ADR 0006)](../adr/0006-browser-customer-lifecycle.md), [Verified enterprise associations (ADR 0007)](../adr/0007-verified-enterprise-associations.md), [Operator identity (ADR 0008)](../adr/0008-operator-identity.md).
- [Current M1 verification](../development/m1-completion.md), [EU-first security limits](../security/eu-first.md) and [roadmap](roadmap.md).
