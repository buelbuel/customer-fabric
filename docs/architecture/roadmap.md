# Customer Fabric roadmap

**Canonical planning source:** [GitHub roadmap index #8](https://github.com/buelbuel/customer-fabric/issues/8), which links the stage epics and actionable, acceptance-tested work items. This file is a short entry point, not a parallel issue backlog.

| Stage | Status and scope | GitHub |
| --- | --- | --- |
| M0 | Implemented/verified for synthetic development: profiles, ownership, provenance, SQL transactions, outbox record, fixtures and CI | [Baseline](../development/verification.md) |
| M1 | Implemented/merged for synthetic development: Kratos self-service, customer subject profile anchors, verified external associations, named operator MFA | [Verification](../development/m1-completion.md) |
| Foundation hardening | In progress: flaky Kratos concurrency test, published image risks, current docs and CI release hygiene | [Epic #9](https://github.com/buelbuel/customer-fabric/issues/9) |
| M2 | Planned: purposes/legal bases, consent evidence/withdrawal, rights, retention, erasure and replay fences | [Epic #10](https://github.com/buelbuel/customer-fabric/issues/10) |
| M3 | Planned: Kafka event backbone, Temporal workflows, import/event ingestion, source ordering, destinations, connectors and reconciliation | [Epic #11](https://github.com/buelbuel/customer-fabric/issues/11) |
| M4 | Planned: explainable match candidates, PostgreSQL-first segmentation and consent-aware activation | [Epic #12](https://github.com/buelbuel/customer-fabric/issues/12) |
| M5 | Planned: production authentication, TLS/secrets, EU operational controls, DB privileges, backup/restore, audit and security gates | [Epic #13](https://github.com/buelbuel/customer-fabric/issues/13) |
| v1 beta → GA | Planned: self-hosted install/upgrade, API compatibility, docs, release/security policy, pilots and support | [Epic #14](https://github.com/buelbuel/customer-fabric/issues/14) |
| Customer Fabric Cloud | Planned: shared management control plane and isolated tenant data planes | [Epic #90](https://github.com/buelbuel/customer-fabric/issues/90) |
| Beyond v1 | Research only: matching/merge, federation/passkeys, analytics, messaging, connectors, hosted options and journeys; no implied commitment | [Discovery #15](https://github.com/buelbuel/customer-fabric/issues/15) |

## Delivery principles

- Stage names are dependencies and release gates, **not dates**. M2–M4 may be built with synthetic data while M5 production controls remain open.
- Do not confuse consent with verified email, a linking grant, marketing preference or a lawful basis. Do not auto-merge profiles by email.
- Kafka and Temporal are approved target infrastructure for M3, and neither is implemented. Hydra is a candidate pending its own review. ClickHouse, visual authentication builders, mandatory client SDKs and multi-brand features remain out of scope without reviewed evidence and an ADR.
- Self-hosted Customer Fabric never depends on Customer Fabric Cloud. Cloud is a separate track with isolated tenant data planes and its own release gates ([epic #90](https://github.com/buelbuel/customer-fabric/issues/90)).
- Keep development and production readiness distinct. **No real personal data** before production security, privacy/legal/controller review, vulnerability triage, data lifecycle and tested restore gates pass.
- Changes are delivered by focused PR, independent review and CI. Release artifacts and supported APIs require explicit v1 compatibility policy.
