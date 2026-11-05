# Roadmap

- M0: secure development profile API, PostgreSQL transactions, external identifiers, first-writer ownership, provenance, replay receipts, durable outbox, read-only operations console.
- M1: Ory Kratos identities, real self-service flows, verified linking, and separate administrative authentication.
- M2: purpose/version-aware consent, withdrawal, evidence, export, erasure and retention operations.
- M3: evaluate River licensing and operational fit; durable jobs, webhooks, integration retries and reconciliation.
- M4: PostgreSQL-first segmentation, time-based evaluation, consent-aware activation.
- M5: EU deployment, least-privilege roles, TLS, secrets management, monitoring, backup/restore drills and pilot readiness.

M0 is development-only. Future milestones are not implemented functionality. Kafka, Temporal, Hydra and ClickHouse require a concrete requirement and a new ADR. Dedicated customer deployments precede shared multi-tenancy. No SDKs, visual builders or cloud billing are planned for this foundation.
