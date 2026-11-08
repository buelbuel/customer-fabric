# Roadmap — product and cloud architecture

**Canonical planning and acceptance criteria:** [GitHub roadmap #8](https://github.com/buelbuel/customer-fabric/issues/8). This summary describes the **target product**, not what is running in M1. All current live development tests use synthetic data. Nothing below authorizes production use.

| Milestone | Product scope | Work |
| --- | --- | --- |
| M0 | Profile persistence, first-writer attribute ownership, provenance, idempotency, database transactions and outbox records | Implemented development baseline |
| M1 | Customer Kratos registration/login/recovery, stable profile subject, independently verified external associations, isolated named operator Kratos with TOTP | Implemented development baseline; not a complete OIDC provider |
| Product core | First-class OIDC applications/clients, customer-owned business profiles, governed Customer 360, typed source schemas and privacy-aware event-collection decision | [Product feature gap epic #71](https://github.com/buelbuel/customer-fabric/issues/71) |
| M2 | Versioned purposes/lawful bases/consent evidence and effective policy, rights/export/erasure/replay fences | [Epic #10](https://github.com/buelbuel/customer-fabric/issues/10) |
| M3 | **Kafka first-class event backbone**, **Temporal first-class durable workflow engine**, ingestion, connector contracts, event ordering, transactional outbox relay, reconciliation and destinations | [Epic #11](https://github.com/buelbuel/customer-fabric/issues/11), [Kafka+Temporal #64](https://github.com/buelbuel/customer-fabric/issues/64), [outbox delivery #36](https://github.com/buelbuel/customer-fabric/issues/36) |
| M4 | Explainable match candidates, PostgreSQL-first typed segmentation, time-aware audience membership and consent-safe activation | [Epic #12](https://github.com/buelbuel/customer-fabric/issues/12) |
| M5 | Production authentication/RBAC, TLS/secrets, database permissions, monitoring, vulnerabilities, encrypted restore/replay fences and EU operational controls | [Epic #13](https://github.com/buelbuel/customer-fabric/issues/13) |
| Self-hosted v1 | Install/upgrade, migration/compatibility, documentation, releases, independent security review and pilots | [Epic #14](https://github.com/buelbuel/customer-fabric/issues/14) |
| **Customer Fabric Cloud** | Shared SaaS control plane, shared staff identity/membership and billing; tenant-isolated Customer Fabric data/identity/OIDC/keys with per-tenant PG databases, Kafka ACLs and Temporal workflows | [Cloud epic #90](https://github.com/buelbuel/customer-fabric/issues/90) and [issues #91–#100](https://github.com/buelbuel/customer-fabric/issues/91) |
| Later discovery | Advanced merging, ClickHouse, extension ecosystem, pricing, journeys and experimentation | [Discovery #15](https://github.com/buelbuel/customer-fabric/issues/15) |

## Accepted architecture direction

- **Go modular monolith** for Customer Fabric domain/API, plus operational Go processes for event relaying and Temporal workers; PostgreSQL continues as the transactional source of truth.
- **Kafka transports events**; **Temporal orchestrates long-running workflows**; they do not replace the atomic PostgreSQL profile/provenance/outbox transaction or double-write business operations.
- **Cloud control plane**: one shared management database for organizations, staff memberships, plans/billing and deployment references; a separate shared staff Kratos identity database.
- **Cloud tenant data plane**: one isolated PostgreSQL logical *database* per customer organization, separate customer Kratos data and tenant-specific OIDC issuer/client state if approved, tenant-bound Go API/workers, Kafka topics/ACLs and Temporal namespace/task queues. The first five tenants can share a separate tenant-data PostgreSQL cluster; regulated/high-volume customers can receive dedicated instances.
- **Controller**: one approved controller configuration per dedicated tenant deployment. The Cloud operator is not by default the single controller for all companies. Cloud customer records do not belong in shared management tables.
- Self-hosted single-organization deployments remain independent of Cloud billing/control plane; Cloud is a distinct managed-service deployment track.
- No client SDKs, WYSIWYG form builder, arbitrary theming or multibrand requirements have been adopted. OIDC via Hydra is a **target candidate subject to protocol, self-hosted licensing and conformance review**, not an implemented M1 service.
- ClickHouse remains optional research. Kafka and Temporal are **explicitly included**, not optional product discovery, but infrastructure does not need to run in every developer's lightweight M0 unit-test environment.

## Architecture details and invariants

- [Control and isolated tenant data planes](cloud-control-plane.md)
- [Kafka events and Temporal workflows](eventing-workflows.md)
- [Initial application/DB architecture](overview.md)
- [Accepted data lifecycle design](../adr/0003-data-lifecycle.md)

**Release gates:** Cloud must pass tenant/issuer/topic/namespace isolation, per-tenant restore/retention and cross-tenant malicious input tests. Self-hosted and Cloud each require separately demonstrated production readiness; a green synthetic CI run is not production approval.
