# Roadmap

- M0 is verified at `da16574`. It covers the development profile API, transaction invariants and operations console. [GitHub Actions](https://github.com/buelbuel/customer-fabric/actions/runs/42248344176)
- M1 adds customer account lifecycle, verified enterprise associations and named MFA operator access for synthetic development. The [verification report and M2 handoff](../development/m1-completion.md) track evidence, remaining acceptance checks and release restrictions.
- M2: purpose/version-aware consent, withdrawal, evidence, export, erasure and retention operations.
- M3: evaluate River licensing and operational fit; durable jobs, webhooks, integration retries and reconciliation.
- M4: PostgreSQL-first segmentation, time-based evaluation, consent-aware activation.
- M5: EU deployment, least-privilege roles, TLS, secrets management, monitoring, backup/restore drills and pilot readiness.

M0 is development-only. Future milestones are not implemented functionality. Kafka, Temporal, Hydra and ClickHouse require a concrete requirement and a new ADR. Dedicated customer deployments precede shared multi-tenancy. No SDKs, visual builders or cloud billing are planned for this foundation.
