# ADR 0014: Temporal workflows started from relayed profile events

Status: accepted (maintainer direction of 2026-11-10, clarified 2026-11-11, #64). This is the production-intended path for external change notifications: synthetic destinations are test fixtures, not a product mode. Processing real customer data stays disallowed until the separate security, privacy and operational readiness gates are met.

PostgreSQL outbox → relay ([ADR 0013](0013-event-transport.md)) → Kafka → consumer → Temporal workflow → activity authorizing against PostgreSQL → subscribed destination → recorded outcome.

## Decision

- **First workflow: the profile-change notification.** The workflow tells each destination whose **notification subscription** covers the event that a source record changed. The notification carries the event envelope only: identifiers, type, tenant, source, version and time, never attribute values. A receiver reads current, authorized state from Customer Fabric's API.
- **Notification subscriptions are the authorization.** An allowlisted host and an active record are not enough. A subscription, created by `privacy_admin`, names:
  - the registered destination;
  - the processing purpose, which must have an active policy version recording its lawful basis;
  - whether the recipient is a controller or a processor;
  - the permitted event types;
  - the data categories, which can only be `record_reference`, because nothing else is ever sent;
  - the sources whose records are in scope.

  The `basis_assessment` is recorded as the approving `privacy_admin`'s attestation that the assessment exists and is approved; Customer Fabric does not verify the assessment itself.

  A subscription is approved under the purpose's active policy version and authorizes nothing once another version is active; it cannot be resumed then, and a new subscription must be approved. Under a non-consent lawful basis it must cite the approved assessment behind it (for example a legitimate-interests assessment): the label of a basis alone authorizes nothing. Its scope never changes; a different scope is a new subscription. Each subscription is notified and recorded separately.
- **The activity is the final dispatch point.** Immediately before every attempt, including every retry, it decides again from one consistent snapshot of committed PostgreSQL state. A notification is suppressed, with a reason code, unless all of these hold:
  - **The subscription** exists, is active, and covers the event's type and source.
  - **The record** exists, is active (not erasure pending or erased) and is not restricted.
  - **The lawful basis.** The purpose's active policy version is the one the subscription was approved under, and the record's subjects permit the purpose under its lawful basis. A record has one lifetime per identifier, and **every lifetime must permit**:
    - A lifetime verifiably associated with a customer now follows that customer's own effective permission, which already includes protective decisions made on the record. The customer must also be active and not restricted.
    - Any other lifetime, including one whose association was revoked, follows the evidence on its own record subject.
    - A record created without any identifier has no lifetime, and so nothing that could permit the purpose; it is never notified (`record_without_identifier`). Under consent, an unlinked record is not notified today: an operator's grant on a record does not count as consent, and verified consent from enterprise source systems is not ingested yet. A governed source-consent ingestion model (proof, purpose and version, controller identity, source authority, subject binding, withdrawal and replay protection) is the next change, evaluated by the same engine.
    - Under legitimate interests or public task, a record is notified unless its subject objected.
    - Missing or ambiguous authorization fails closed.
  - **The destination** is registered and its URL is on the webhook allowlist.
  - Sends reuse registered destinations: allowlisted hosts and an HMAC signature, with the event ID as the `Idempotency-Key`.
  - Failures:
    - transport errors, 408, 429 and 5xx from the destination are retried with backoff, up to five attempts, and then recorded as `failed` (`retries_exhausted`). The workflow counts these attempts itself, so only the destination's failures use them up;
    - any other 4xx is a rejection, recorded as `failed` (`rejected`) without retrying;
    - Customer Fabric's own failures, such as its database being unreachable or a send cut short by shutdown, are retried until they clear and count against nothing, so an outage never consumes an event. Recording the outcome is retried the same way. A lasting failure of this kind (for example a defect) is retried every five minutes indefinitely and is visible only in Temporal, until monitoring exists.
  - **Every recorded outcome is final**, including `suppressed`: an event suppressed because its destination was not registered is not sent when the destination is registered later. There is no redrive yet.
- **Exactly-once effect from at-least-once parts.**
  - The consumer starts the workflow under the ID `profile-change-<event id>`, with duplicate IDs rejected, so a redelivered event does not run twice while Temporal remembers the ID.
  - The outcome is written once per event and subscription to `notification_deliveries`, with the destination's attempts and last status code. A redelivery after Temporal forgot the workflow finds that row and is not sent again.
  - A crash between a send and its record can resend the same event; the destination deduplicates by `Idempotency-Key`.
- **The consumer never skips an event.**
  - It commits a Kafka offset only after the record's workflow was started or found already started.
  - While Temporal is unreachable, it retries the same record and holds its place.
  - It refuses envelopes with an unknown schema or another tenant, logging a reason code and never the payload. Those records are committed, not retried.
- **Workflow state holds references only.** Workflow input and history contain the envelope, never customer values. History and visibility are still pseudonymous personal data (#28).
- **Opt-in and in-process.** The API process runs the Temporal worker and the consumer only when `CF_TEMPORAL_ADDRESS` is set. That requires the relay (`CF_KAFKA_BROKERS`) and `CF_TENANT_ID`. Where notifications go is decided by subscriptions, not configuration.
  - Startup does not need Temporal. The client connects lazily. A Temporal worker that fails to start cannot be started again, so a new one is built for every attempt until one starts.
  - The namespace defaults to `customer-fabric` (`CF_TEMPORAL_NAMESPACE`). The task queue is `profile-changes`.
- **Dependencies.**
  - Temporal Go SDK `go.temporal.io/sdk` v1.50.0 (MIT). With `golang.org/x/net` raised to v0.62.0, every module it links is Apache-2.0, BSD, MIT or ISC licensed, and `govulncheck` finds no vulnerabilities.
  - Local server: the Temporal CLI development server `temporalio/temporal:1.10.0` (MIT), pinned by digest, headless, in memory, on the internal events network. 1.9.1 was rejected: its binary had 1 CRITICAL and 17 HIGH findings. 1.10.0 scans with medium findings only.
- **Where it runs.** `deploy/compose.events.yaml` adds Temporal next to Kafka. `make events-test` and `make events-up` start both, `make events-scan` scans both images, and the separate `eventing` CI job runs them. Neither `make up`, `make test` nor the required checks need Temporal. The workflow logic, the consumer and the notifier are tested in `make test` without it.

## Not decided here

- **Races with a withdrawal.** The decision is taken in one snapshot immediately before the send, so a withdrawal or restriction committed between that snapshot and the send, or while the request is in flight, is not seen by that attempt. The check cannot recall a request already in flight when consent is withdrawn or a record is restricted. No downstream suppression or correction protocol (for example, a withdrawal notice to recipients) is approved yet. Until one is, recipients must treat notifications as references to look up under current authorization, and Customer Fabric does not claim deletion from recipients.
- **Egress hardening** beyond the host allowlist: resolving and checking the destination's IP addresses at connection time (DNS rebinding) and requiring TLS outside development. This applies to activation deliveries too and follows as its own change.
- **Destination secret protection** at rest: signing secrets are stored unencrypted in PostgreSQL today, as for activation destinations.
- **Production Temporal.** Persistence, a namespace and credentials per tenant (#97), retention and archival, mTLS, and upgrades. The development server is in memory and unauthenticated, for synthetic data only.
- **Further workflows** (connector imports, consent propagation, DSAR, erasure acknowledgements, Cloud provisioning). Each follows this pattern: references in the workflow, decisions in activities from current state.
- **A dead-letter topic** for refused envelopes. Today they are logged with a reason code and skipped.
- **Retention values and pruning.** Every store and flow of this path is registered in the [data inventory](../security/data-inventory.md). The retention values for Kafka, Temporal histories, the outbox and notification outcomes, and their pruning, belong to the privacy retention rules (ADR 0003, #28).

## Alternatives considered

- **Calling the destination from the Kafka consumer:** no durable retries, backoff or visibility per event without rebuilding what Temporal provides.
- **A separate worker binary or service:** more deployment surface for a single workflow. The modular monolith can split it out later without changing the workflow.
- **The Temporal server images with an external database:** closer to production but heavier to run locally. Deferred to the production decision above.
