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
    - A change therefore carries an earlier epoch than a subscription change exactly when it committed first, however their transactions overlap. Writes never wait for each other. A subscription change waits for the slowest write in flight, such as an import chunk, and writes that start meanwhile wait for it in turn, within the bounds below.
    - Rows that existed before the fence keep the decisions their times gave.
    - **Bounded** (migration 037, `CF_FENCE_CHANGE_WAIT`, `CF_FENCE_WRITE_WAIT`, `CF_TRANSACTION_TIMEOUT`; defaults 10 s, 15 s and 30 s):
      - A subscription change waits for a lock, the fence included, at most the change wait. Then it is refused as 503 with `Retry-After`, nothing is changed, and the writes queued behind it go on.
      - A write waits for any lock, the fence and the row and key locks after it, at most the write wait, and is refused the same way. Keep it above the change wait, so a write queued behind a change that gives up is not refused at the same moment. Background workers such as the identity reconciler treat the refusal as any failed attempt: the transaction rolls back and the next tick retries.
      - Every transaction that writes the outbox or changes a subscription has a deadline the database enforces from its first statement (`transaction_timeout`, PostgreSQL 17 or later), so a stuck or vanished client cannot hold the fence past it. A transaction that reaches it is ended, never committed, and is refused as 503.
      - A subscription change therefore waits no longer than the shorter of the change wait and the longest write in flight, and a write waits no longer than the change wait plus the change's own short transaction.
    - **Suspension takes effect first:** suspending an active subscription sets `delivery_halted_at` in a transaction of its own, before it waits for the fence, and the notifier sends nothing to a subscription with it set (`subscription_suspended`).
      - **Completed or pending:** if the suspension then completes the answer is 200 and the subscription is `suspended`. If it cannot (the writes in flight do not commit within the change wait, or a lock or deadline intervenes) the answer is 202, not an error: the subscription is returned as it is, `active` with `delivery_halted_at`, and the request is audited as the operator's change (status 202). Only a refusal, such as an unknown or superseded subscription, is an error.
      - **Never left unresolved:** repeating the suspension completes it, and resuming cancels it. Otherwise the housekeeper completes it once it has been pending 30 s (never one resumed meanwhile, decided under the purpose's lock), retrying every minute, and logs an error for any suspension pending five minutes. `GET /v1/operator/commit-fence` reports their count and the oldest's start, to alert on.
      - A notification already past its decision when the halt commits can still be sent, as with any suspension. A client that retries changes in a loop while a long write is in flight can keep new writes waiting for up to the change wait at a time.
    - **Isolation:** advisory locks are database-wide, so the fence is keyed by schema. A subscription change never waits for, or holds up, the writes of another schema of the same database, and tenant data planes in separate databases (the Cloud target) share nothing. Within one data plane a change still orders against that plane's writes, for the bounded time above; there is no finer scope than the data plane, because a change decides which of its subscriptions cover every change.
    - **Measured:** `GET /v1/operator/commit-fence` (audit readers) reports, per API process, how many writes and subscription changes waited for the fence, how many waited 50 ms or more (blocked) or gave up, the wait's total, longest and histogram, and the limits in force. Waits of a second or more, and every give-up, are also logged as warnings with durations only.
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
