# ADR 0014: Temporal workflows started from relayed profile events

Status: accepted for synthetic development (maintainer direction of 2026-11-10, #64). It completes the synthetic vertical slice of the [event and workflow target](../architecture/eventing-workflows.md):
PostgreSQL outbox → relay ([ADR 0013](0013-event-transport.md)) → Kafka → consumer → Temporal workflow → activity re-checking PostgreSQL → synthetic destination → recorded outcome.

## Decision

- **First workflow: the profile-change notification.** The workflow tells one registered destination (`CF_WORKFLOW_DESTINATION`) that a source record changed. The notification carries the event envelope only: identifiers, type, tenant, source, version and time, never attribute values. A receiver reads current, authorized state from Customer Fabric's API.
- **The activity is the final dispatch point.** Immediately before every attempt it decides again from committed PostgreSQL state:
  - A record that is no longer active (erasure pending or erased) or is restricted is suppressed.
  - So is an event whose destination is unregistered or whose host is not in the webhook allowlist.
  - Sends reuse registered destinations: allowlisted hosts and an HMAC signature, with the event ID as the `Idempotency-Key`.
  - Failures:
    - transport errors, 408, 429 and 5xx are retried with backoff, up to five attempts;
    - any other 4xx is a rejection and is not retried.
- **Exactly-once effect from at-least-once parts.**
  - The consumer starts the workflow under the ID `profile-change-<event id>`, with duplicate IDs rejected, so a redelivered event does not run twice while Temporal remembers the ID.
  - The outcome is written once to `profile_change_notifications`. A redelivery after Temporal forgot the workflow finds that row and is not sent again.
  - A crash between a send and its record can resend the same event; the destination deduplicates by `Idempotency-Key`.
- **The consumer never skips an event.**
  - It commits a Kafka offset only after the record's workflow was started or found already started.
  - While Temporal is unreachable, it retries the same record and holds its place.
  - It refuses envelopes with an unknown schema or another tenant, logging a reason code and never the payload. Those records are committed, not retried.
- **Workflow state holds references only.** Workflow input and history contain the envelope, never customer values. History and visibility are still pseudonymous personal data (#28).
- **Opt-in and in-process.** The API process runs the Temporal worker and the consumer only when `CF_TEMPORAL_ADDRESS` is set. That requires the relay (`CF_KAFKA_BROKERS`), `CF_TENANT_ID` and `CF_WORKFLOW_DESTINATION`.
  - Startup does not need Temporal: the client connects lazily and the worker retries.
  - The namespace defaults to `customer-fabric` (`CF_TEMPORAL_NAMESPACE`). The task queue is `profile-changes`.
- **Dependencies.**
  - Temporal Go SDK `go.temporal.io/sdk` v1.50.0 (MIT). With `golang.org/x/net` raised to v0.62.0, every module it links is Apache-2.0, BSD, MIT or ISC licensed, and `govulncheck` finds no vulnerabilities.
  - Local server: the Temporal CLI development server `temporalio/temporal:1.10.0` (MIT), pinned by digest, headless, in memory, on the internal events network. 1.9.1 was rejected: its binary had 1 CRITICAL and 17 HIGH findings. 1.10.0 scans with medium findings only.
- **Where it runs.** `deploy/compose.events.yaml` adds Temporal next to Kafka. `make events-test` and `make events-up` start both, `make events-scan` scans both images, and the separate `eventing` CI job runs them. Neither `make up`, `make test` nor the required checks need Temporal. The workflow logic, the consumer and the notifier are tested in `make test` without it.

## Not decided here

- **Production Temporal.** Persistence, a namespace and credentials per tenant (#97), retention and archival, mTLS, and upgrades. The development server is in memory and unauthenticated, for synthetic data only.
- **Further workflows** (connector imports, consent propagation, DSAR, erasure acknowledgements, Cloud provisioning). Each follows this pattern: references in the workflow, decisions in activities from current state.
- **A dead-letter topic** for refused envelopes. Today they are logged with a reason code and skipped.
- **Event retention in the privacy register.** Kafka retention (ADR 0013), Temporal history retention and the notification rows are data categories for the privacy retention rules (ADR 0003, #28).

## Alternatives considered

- **Calling the destination from the Kafka consumer:** no durable retries, backoff or visibility per event without rebuilding what Temporal provides.
- **A separate worker binary or service:** more deployment surface for a single workflow. The modular monolith can split it out later without changing the workflow.
- **The Temporal server images with an external database:** closer to production but heavier to run locally. Deferred to the production decision above.
