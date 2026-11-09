# Customer Fabric roadmap

**Canonical planning source:** [roadmap #8](https://github.com/buelbuel/customer-fabric/issues/8) and its stage epics. This file shows what works now and what comes next; it is not a second backlog. Everything runs on synthetic data only.

## Status

| Stage | Status | Scope | GitHub |
| --- | --- | --- | --- |
| M0 | **Works** | Profiles, attribute ownership, provenance, idempotent writes, SQL transactions, outbox record, fixtures, CI | [CI at `da16574`](https://github.com/buelbuel/customer-fabric/actions/runs/42248344176) |
| M1 | **Works** | Kratos customer self-service, customer profile anchor, verified source-record associations, named operator MFA | PRs [#5](https://github.com/buelbuel/customer-fabric/pull/5), [#6](https://github.com/buelbuel/customer-fabric/pull/6), [#7](https://github.com/buelbuel/customer-fabric/pull/7); [CI at `14d708e`](https://github.com/buelbuel/customer-fabric/actions/runs/42381371758) |
| Foundation | In review | Deterministic Kratos race check, TCP PostgreSQL health check, current docs | [#9](https://github.com/buelbuel/customer-fabric/issues/9) |
| M2 consent | **Works** | Purposes and policy versions, consent evidence, effective permission, and customer grant and withdrawal (#105–#107, [ADR 0011](../adr/0011-customer-consent-api.md)) | [#10](https://github.com/buelbuel/customer-fabric/issues/10) |
| M2 rights | Blocked on ADR approval | Erasure, retention, legal holds, restore protection | [#10](https://github.com/buelbuel/customer-fabric/issues/10), PR #101 |
| M3 | Planned | Source event ingestion, ordering, destinations; then Kafka and Temporal | [#11](https://github.com/buelbuel/customer-fabric/issues/11) |
| M4 | Planned | PostgreSQL-first segmentation and consent-gated activation | [#12](https://github.com/buelbuel/customer-fabric/issues/12) |
| M5 | Planned | Production authentication, TLS and secrets, DB privileges, backup and restore, security review | [#13](https://github.com/buelbuel/customer-fabric/issues/13) |
| v1 | Planned | Self-hosted install and upgrade, API compatibility, releases, pilots | [#14](https://github.com/buelbuel/customer-fabric/issues/14) |
| Cloud | Planned, separate track | Shared control plane, isolated tenant data planes | [#90](https://github.com/buelbuel/customer-fabric/issues/90) |
| Beyond v1 | Research only | Matching and merge, analytics, connectors ecosystem, journeys | [#15](https://github.com/buelbuel/customer-fabric/issues/15) |

## Next working milestones

1. **Source events into a usable profile:** an authenticated source sends change events that update the profile bound to its own record, idempotently and in order. `POST /v1/source-events` delivers this; connector receivers, deletes and the rest of #84 remain ([#34](https://github.com/buelbuel/customer-fabric/issues/34), [#84](https://github.com/buelbuel/customer-fabric/issues/84), [#35](https://github.com/buelbuel/customer-fabric/issues/35)).
2. **Segment and activate:** define a segment, preview it, and deliver consenting members to a test webhook, excluding anyone who withdrew ([#42](https://github.com/buelbuel/customer-fabric/issues/42), [#43](https://github.com/buelbuel/customer-fabric/issues/43), [#44](https://github.com/buelbuel/customer-fabric/issues/44), [#37](https://github.com/buelbuel/customer-fabric/issues/37)).

Each milestone ends with a working, demonstrable flow on the local stack.

## Architecture direction

- Go modular monolith with PostgreSQL as the transactional source of truth. Profile, provenance and outbox writes stay in one transaction.
- Kafka (event transport) and Temporal (long-running workflows) are the approved M3 target; neither is implemented, and the first milestones above run on PostgreSQL alone. Details: [Kafka and Temporal](eventing-workflows.md), [Cloud control plane](cloud-control-plane.md).
- Self-hosted never depends on Cloud. Hydra is a candidate pending its own review. No WYSIWYG builders, mandatory SDKs or multi-brand features.
- No real personal data before the M5 security, privacy, legal and restore gates pass.
