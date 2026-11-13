# ADR 0016: Policy changes, reapproval and recovery of notifications

Status: accepted for implementation (#152, follow-up to #151). Real customer data stays disallowed until the release gates are met.

## Context

A notification subscription (ADR 0014) is bound to the policy version of its purpose that it was approved under. Retiring that version leaves the subscription authorizing nothing, which is correct and stays so: nothing is grandfathered into a new version. Before this change, though:

- the interruption was silent;
- each change during it was recorded as finally suppressed;
- reapproving, by creating a new subscription, could not recover anything missed.

## Decision

- **Impact before the change.** `GET /v1/operator/consent/policy-versions/{id}/impact` lists what the version's next transition does to subscriptions.
  - **Retirement** interrupts every active subscription approved under the version. The response carries an acknowledgment, a digest of the version and of exactly those subscriptions.
  - **Activation** restores none. Subscriptions still needing reapproval are listed as `remains_interrupted`.
  - **Confirming a retirement:** a retirement that interrupts anything needs that acknowledgment as `acknowledged_impact`; a malformed one is 422.
  - **Concurrency:** the transition, subscription creation, resumption, reapproval and recovery requests all take the purpose's lock. A retirement that started from a stale preview, for example after a subscription was added in between, is refused (409).
- **Interruption is a state.** On retirement, those subscriptions become `needs_reapproval` with `interrupted_at`.
  - **Records:** an audit entry records the version and the operator, and the API logs a warning with the count; neither holds values.
  - **Selection by commit time:** an event goes to the subscriptions that covered it when its change committed, however late it is processed: those created by then and active, interrupted, or superseded only after it. A change committed during an interruption is therefore recorded as suppressed on the interrupted subscription (`subscription_needs_reapproval`, or `subscription_superseded` if it is processed after the reapproval) rather than dropped. It never reaches the replacing subscription on its own, even through a lagging consumer, and a new subscription never receives changes committed before it existed. An abandoned (suspended) subscription is not selected for changes committed after its suspension, even once it is superseded.
  - **Commit order, not clocks:** "committed by then" is decided by a commit epoch, never by a timestamp.
    - Every transaction that writes the outbox holds a commit fence shared, from its start to its commit, and stamps its changes with the current epoch. The database refuses an outbox write outside the fence.
    - Creating or suspending a subscription takes the fence exclusively and advances the epoch. It waits for the writes in flight to commit, and writes that start meanwhile wait for it.
    - A change therefore carries an earlier epoch than a subscription change exactly when it committed first, however their transactions overlap. Writes never wait for each other. A subscription change waits for the slowest write in flight, such as an import chunk, and writes that start meanwhile wait for it in turn.
    - Rows that existed before the fence keep the decisions their times gave.
  - **API visibility:** the subscription list shows each interruption's start, end and suppressed count, and `/audit` lists the history.
  - **No resume:** an interrupted subscription cannot be resumed. It can be suspended to abandon it, and then records nothing more; it can still be reapproved later.
  - **Migration:** subscriptions already bound to an inactive version when this was introduced were interrupted and audited (actor `migration`), dated at the migration because the retirement time was not recorded.
- **Reapproval is explicit and version-bound.** `POST /{name}/reapprove` creates a new, named subscription with the same scope under the active version, recording `replaces`. It applies to an interrupted subscription, or a suspended one whose version is no longer active.
  - **Basis assessment:** a non-consent basis needs its assessment cited again.
  - **History kept:** the old subscription becomes `superseded`, and both rows and their audit entries are kept.
  - **Nothing is sent** by reapproval, or by the policy change.
- **Recovery is a separate, authorized request.** `POST /{name}/recoveries`, for privacy admins, with an `Idempotency-Key`, queues what the replaced subscription suppressed for lack of an approved version.
  - **Idempotent:** a repeated key returns the original request, and an event is queued at most once per subscription across all requests.
  - **Bounded:** one request queues at most 10,000 events, and `more_pending` says whether a further request with a new key has more.
  - **Newest change only:** per record only the newest change is queued, because a notification only says that a record changed. Older changes, and changes older than one the subscription already delivered, are `superseded`.
  - **No moving back:** immediately before every attempt, live or recovered, a change older than a version the subscription already delivered for the record is suppressed (`newer_change_notified`). Two sends that run concurrently can still arrive out of order, so recipients order by `profile_version`.
  - **Unrecoverable** (shown explicitly):
    - a change whose outbox reference is gone (`history_unavailable`);
    - a change suppressed more than 30 days ago (`outside_recovery_window`).
- **Recovery reuses the delivery path.** A starter in the eventing process starts one `RecoverNotification` Temporal workflow per queued item.
  - **Workflow:** it runs the same notify activity, retries and destination-attempt counting as `ProfileChange`. Every attempt authorizes again from current state: the subscription and its active policy version, effective consent or objection, the record's lifecycle and restriction, event and source scope, and the destination and its allowlisting.
  - **Exactly-once outcome:** the outcome is recorded once per event and subscription, which stops any resend.
  - **Crash safety:** the workflow ID is derived from subscription and event, so restarting after a crash, or a second process, never starts an item twice. An item that cannot be started stays queued without holding back the others.
  - **Withheld items:** a withdrawn, objected, restricted, erased or out-of-scope record is suppressed with its reason, never sent.
  - **Envelopes:** they are rebuilt from the outbox exactly as the relay published them and carry only references.
- **Minimal history.** Recovery needs nothing beyond existing references:
  - suppression rows (event and profile IDs, version, reason);
  - the outbox reference;
  - recovery items (references and states).

  No values are kept for replay.

## Semantics

- Change notifications are **at least once where authorized**, not a lossless change stream.
  - A change is sent only if it is authorized at the moment of sending.
  - Recipients deduplicate by event ID (`Idempotency-Key`) and order by `profile_version`.
  - A recovered notification can arrive after newer ones about other records.
- A subscription that needs stronger guarantees needs a separately designed contract.

## Consequences

- An operator sees an interruption before causing it, and in the subscription list afterwards. Recovering missed changes needs two explicit privacy-admin decisions: reapproval, then recovery.
- **Retention:**
  - recovery depends on suppression rows and outbox references, both retained indefinitely until #28 sets pruning;
  - the 30-day recovery window bounds what can be recovered, independently of how long rows are kept.
- **Not covered:**
  - an admin-console view of these states (the API exposes them);
  - an external alert channel beyond the log warning.
