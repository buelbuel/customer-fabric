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

  A subscription is approved under the purpose's active policy version and authorizes nothing once that version is retired: it then needs reapproval as a new subscription, and what it missed is sent only on an explicit, re-authorized recovery request ([ADR 0016](0016-policy-change-notification-recovery.md)). Under a non-consent lawful basis it must cite the approved assessment behind it (for example a legitimate-interests assessment): the label of a basis alone authorizes nothing. Its scope never changes; a different scope is a new subscription. Each subscription is notified and recorded separately.
- **The activity is the final dispatch point.** Immediately before every attempt, including every retry, it decides again from one consistent snapshot of committed PostgreSQL state. A notification is suppressed, with a reason code, unless all of these hold:
  - **The subscription** exists, is active, and covers the event's type and source.
  - **The record** exists, is active (not erasure pending or erased) and is not restricted.
  - **The lawful basis.** The purpose's active policy version is the one the subscription was approved under, and the record's subjects permit the purpose under its lawful basis. A record has one lifetime per identifier, and **every lifetime must permit**:
    - A lifetime verifiably associated with a customer now follows that customer's own effective permission, which already includes protective decisions made on the record. The customer must also be active and not restricted.
    - Any other lifetime, including one whose association was revoked, follows the evidence on its own record subject.
    - A record created without any identifier has no lifetime, and so nothing that could permit the purpose; it is never notified (`record_without_identifier`). Under consent, an unlinked lifetime needs a verified source grant ([ADR 0015](0015-source-consent.md)); an operator's grant, an unverified source grant or an opt-in flag in the record never counts.
    - Under legitimate interests or public task, a record is notified unless its subject objected.
    - Missing or ambiguous authorization fails closed.
  - **The destination** is registered and its URL is on the webhook allowlist.
  - Sends reuse registered destinations: allowlisted hosts and an HMAC signature, with the event ID as the `Idempotency-Key`.
  - Every send follows the shared egress policy, as activation deliveries do. It requires https and uses no proxy and no redirects. Each connection's actual address must be public unicast, checked when the socket opens, after name resolution, so a name that is rebound to a private, loopback, link-local or other special address is refused. Such a refusal is final (`destination_address_refused`), not retried. Only local development allows private networks and http, explicitly.
  - Every send is signed with each live secret of its destination. Secrets are sealed at rest under a deployment key. A secret that the configured keys cannot open is never replaced, and nothing is sent unsigned: the activity is retried until the key is back ([key management](../security/webhook-secret-keys.md)).
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
- **Withdrawal notices.** A notification is decided once, just before it is sent. A withdrawal or restriction committed between that decision and the send, or while the request is in flight, is not seen by that attempt, and authorization that ends later is never seen by it at all. The recipient is told instead.
  - **Holding.** A subscription that delivered a change of a record holds it. A notice covers the newest version delivered before it, and a later delivery holds the record again.
  - **Withdrawn.** A held record is withdrawn when it is no longer authorized for the subscription's purpose. The decision is the notifier's own record decision, taken again on current state: the record is unavailable or restricted, the purpose has no active policy version, or its subject does not permit the purpose under the active version. Suspending or interrupting a subscription stops changes; it does not withdraw what was sent.
  - **Review.** A record is reviewed when the same transaction that may end its authorization queues it: a consent decision on its subject or its customer, a verified association created or revoked, a restriction of it or of its customer, and every recorded delivery, which catches a withdrawal that raced the notification. A continuous sweep over every held record covers what no write announces, such as a grant whose verification became overdue or a suspended consent authority.
  - **Notice.** The destination receives a signed `notification.withdrawn.v1` notice (schema `customer-fabric.withdrawal.v1`): the notice ID, which is also its `Idempotency-Key`, the tenant, the subscription, the record, the last version delivered, a reason code and the time. It carries no values.
  - **Delivery.** In the API process, like activation deliveries: decided again immediately before every attempt, cancelled (`authorized_again`) if the record is authorized again by then, retried with backoff for about a day, then failed. A `privacy_admin` redrives failed notices. Every attempt and decision is recorded, append-only, and listed under the subscription.
  - **What it does not claim.** A notice asks the recipient to stop using the record; deletion by recipients is a matter of their contract and is not claimed. Recipients still treat notifications as references to look up under current authorization.
- **Opt-in and in-process.** The API process runs the Temporal worker and the consumer only when `CF_TEMPORAL_ADDRESS` is set. That requires the relay (`CF_KAFKA_BROKERS`) and `CF_TENANT_ID`. Where notifications go is decided by subscriptions, not configuration.
  - Startup does not need Temporal. The client connects lazily. A Temporal worker that fails to start cannot be started again, so a new one is built for every attempt until one starts.
  - The namespace defaults to `customer-fabric` (`CF_TEMPORAL_NAMESPACE`). The task queue is `profile-changes`.
- **Dependencies.**
  - Temporal Go SDK `go.temporal.io/sdk` v1.50.0 (MIT). With `golang.org/x/net` raised to v0.62.0, every module it links is Apache-2.0, BSD, MIT or ISC licensed, and `govulncheck` finds no vulnerabilities.
  - Local server: the Temporal CLI development server `temporalio/temporal:1.10.0` (MIT), pinned by digest, headless, in memory, on the internal events network. 1.9.1 was rejected: its binary had 1 CRITICAL and 17 HIGH findings. 1.10.0 scans with medium findings only.
- **Where it runs.** `deploy/compose.events.yaml` adds Temporal next to Kafka. `make events-test` and `make events-up` start both, `make events-scan` scans both images, and the separate `eventing` CI job runs them. Neither `make up`, `make test` nor the required checks need Temporal. The workflow logic, the consumer and the notifier are tested in `make test` without it.

## Not decided here

- **Recipient obligations.** The contract that obliges a recipient to act on a withdrawal notice, and to delete or confirm deletion, is a release gate (legal), not an engineering decision.
- **Production Temporal.** Persistence, a namespace and credentials per tenant (#97), retention and archival, mTLS, and upgrades. The development server is in memory and unauthenticated, for synthetic data only.
- **Further workflows** (connector imports, consent propagation, DSAR, erasure acknowledgements, Cloud provisioning). Each follows this pattern: references in the workflow, decisions in activities from current state.
- **A dead-letter topic** for refused envelopes. Today they are logged with a reason code and skipped.
- **Retention values and pruning.** Every store and flow of this path is registered in the [data inventory](../security/data-inventory.md). The retention values for Kafka, Temporal histories, the outbox and notification outcomes, and their pruning, belong to the privacy retention rules (ADR 0003, #28).

## Alternatives considered

- **Calling the destination from the Kafka consumer:** no durable retries, backoff or visibility per event without rebuilding what Temporal provides.
- **A separate worker binary or service:** more deployment surface for a single workflow. The modular monolith can split it out later without changing the workflow.
- **The Temporal server images with an external database:** closer to production but heavier to run locally. Deferred to the production decision above.
