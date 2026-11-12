# Customer Fabric roadmap

**Canonical planning source:** [roadmap #8](https://github.com/buelbuel/customer-fabric/issues/8) and its stage epics. This file shows what works now and what comes next; it is not a second backlog. Everything runs on synthetic data only.

## Status

| Stage | Status | Scope | GitHub |
| --- | --- | --- | --- |
| M0 | **Works** | Profiles, attribute ownership, provenance, idempotent writes, SQL transactions, outbox record, fixtures, CI | [CI at `da16574`](https://github.com/buelbuel/customer-fabric/actions/runs/42248344176) |
| M1 | **Works** | Kratos customer self-service, customer profile anchor, verified source-record associations, named operator MFA | PRs [#5](https://github.com/buelbuel/customer-fabric/pull/5), [#6](https://github.com/buelbuel/customer-fabric/pull/6), [#7](https://github.com/buelbuel/customer-fabric/pull/7); [CI at `14d708e`](https://github.com/buelbuel/customer-fabric/actions/runs/42381371758) |
| Foundation | In review | Deterministic Kratos race check, TCP PostgreSQL health check, current docs | [#9](https://github.com/buelbuel/customer-fabric/issues/9) |
| M2 consent | **Works** | Purposes and policy versions, consent evidence, effective permission, customer grant and withdrawal ([ADR 0011](../adr/0011-customer-consent-api.md)), governed source consent with vetted authorities, signed receipts and evidence verification ([ADR 0015](../adr/0015-source-consent.md), [ADR 0017](../adr/0017-source-consent-trust.md)), processing restrictions, preferences | [#10](https://github.com/buelbuel/customer-fabric/issues/10) |
| M2 rights | Blocked on ADR approval | Erasure, retention, legal holds, restore protection | [#10](https://github.com/buelbuel/customer-fabric/issues/10), PR #101 |
| M3 | In progress | **Works:** source events and CSV imports with ordering and idempotency; the outbox relay to Kafka and Temporal workflows that notify subscribed webhooks, both opt-in ([ADR 0013](../adr/0013-event-transport.md), [ADR 0014](../adr/0014-temporal-workflows.md), [ADR 0016](../adr/0016-policy-change-notification-recovery.md)). **Remaining:** connectors, downstream corrections, retention and pruning | [#11](https://github.com/buelbuel/customer-fabric/issues/11), [#64](https://github.com/buelbuel/customer-fabric/issues/64) |
| M4 | In progress | **Works:** match candidates and reviews, governed segments, snapshots, durable audience jobs and consent-gated activation to webhooks. **Remaining:** merge and unmerge decisions, analytics-grade segment scale | [#12](https://github.com/buelbuel/customer-fabric/issues/12) |
| M5 | Planned | Production authentication, TLS and secrets, DB privileges, backup and restore, security review | [#13](https://github.com/buelbuel/customer-fabric/issues/13) |
| v1 | Planned | Self-hosted install and upgrade, API compatibility, releases, pilots | [#14](https://github.com/buelbuel/customer-fabric/issues/14) |
| Cloud | Planned, separate track | Shared control plane, isolated tenant data planes | [#90](https://github.com/buelbuel/customer-fabric/issues/90) |
| Beyond v1 | Research only | Matching and merge, analytics, connectors ecosystem, journeys | [#15](https://github.com/buelbuel/customer-fabric/issues/15) |

## Next working milestones

1. **One pre-release implementation:** a single development stack with the product authorization model, a baseline schema and no compatibility code for unreleased formats ([#161](https://github.com/buelbuel/customer-fabric/issues/161)).
2. **Reliable external notifications:** sealed signing secrets, downstream corrections to recipients, enforced retention and pruning, and commit ordering ([#64](https://github.com/buelbuel/customer-fabric/issues/64)).
3. **Privacy rights:** legal holds, erasure and retention with restore protection ([#30](https://github.com/buelbuel/customer-fabric/issues/30), [#27](https://github.com/buelbuel/customer-fabric/issues/27), [#28](https://github.com/buelbuel/customer-fabric/issues/28), PR #101).

Each milestone ends with a working, demonstrable flow on the local stack.

## Architecture direction

- Go modular monolith with PostgreSQL as the transactional source of truth. Profile, provenance and outbox writes stay in one transaction.
- Kafka (event transport) and Temporal (durable workflows) run as an opt-in local overlay (`deploy/compose.events.yaml`) behind the PostgreSQL outbox; everything else runs on PostgreSQL alone. Details: [Kafka and Temporal](eventing-workflows.md), [Cloud control plane](cloud-control-plane.md).
- Self-hosted never depends on Cloud. Hydra issues OAuth tokens for registered applications ([ADR 0012](../adr/0012-oidc-provider.md)). No WYSIWYG builders, mandatory SDKs or multi-brand features.
- No real personal data before the M5 security, privacy, legal and restore gates pass.
