# ADR 0010: Recovery journal storage and protocol

Status: **proposed, 2026-11-08. M2 code stays blocked until this ADR and [ADR 0009](0009-m2-consent-subjects-and-lifecycle.md) are approved.** Refines sections 9 and 13 of ADR 0009. Nothing here is implemented.

## Context

ADR 0009 needs lifecycle facts to survive a restore of the database they protect. The journal and PostgreSQL are separate systems, so **no atomic commit across them exists**. Every step below assumes a crash can occur between any two operations.

## Storage decision

**Accepted with conditions:** independent append-only storage outside the tenant's PostgreSQL recovery domain, behind one interface for self-hosted and Cloud.

Independence solves one problem, a restore rewinding the record of what must be reapplied. It does not make the journal safe by itself:

| Risk | Consequence | Control |
| --- | --- | --- |
| New dependency | An outage stops erasures, holds and dispatch | Availability behavior below. Withdrawals are never blocked. |
| Journal loss or corruption | Restore protection is gone | Dispatch and erasure stop. Hash chain detects tampering. Re-baseline below. |
| Tail truncation or a rolled-back journal | Acknowledged facts disappear | The database stores the acknowledged length. A shorter journal stops dispatch. |
| Two timelines writing | Abandoned processes corrupt the record | Generation-conditional append. |
| Append-only versus retention | The journal holds pseudonymous personal data and cannot be kept forever | Deletion only of whole entries past the retention horizon: the oldest restorable backup plus a margin. |
| Cross-tenant access | One journal credential could read another tenant | Tenant binding comes from the credential, never a parameter. Per-tenant encryption keys. |
| Lowest-common-denominator interface | A weak backend silently breaks guarantees | Conformance suite below. A backend that fails is ineligible. |

**Required interface behavior**, verified by a conformance suite every backend must pass:

- Appends are durable when acknowledged and conditional on the current generation.
- Appends are idempotent by event ID, and a repeated ID with a different body is rejected.
- Reads return every acknowledged entry in order.
- Generation advance is an atomic compare-and-set.
- Entries are never modified. Deletion removes only whole entries past the horizon.
- Entries are hash-chained.

**Eligible backends:** a PostgreSQL instance with independent storage and backups, or object storage with conditional writes and retention locks. **Not eligible:** a database on the tenant's cluster, the Cloud control plane, Kafka, Temporal, or a file on the database host unless the operator attests to an independent failure domain. Recommended: separate PostgreSQL for self-hosted and development, object storage in a separate account for Cloud. A PostgreSQL backend enforces append-only by grants and triggers, which is weaker against an administrator than retention locks.

## Protocol

A subject's **scope** is the profiles, records and consent subject locked under ADR 0009 section 8.

1. **Ordering by event type.**

   | Event | Order |
   | --- | --- |
   | Erasure acceptance, refusal, intent, holds, restrictions, fence clears | Journal first, then commit |
   | Withdrawals, objections, denials, unlinks, revocations | Commit first, then journal |

2. **Sequence.** Each event takes a monotonic sequence inside its transaction, after the scope locks are held. Restore advances the sequence past everything in the journal, so a sequence value is never reused across generations.
3. **Transaction fence.** Every transaction re-reads generation and quarantine and aborts on a change.
4. **Reconcile first.** A new decision on a scope first replays any journal entry the database does not reflect, under the same locks. No hold or refusal can therefore follow an unresolved intent.
5. **Roll forward.** An in-doubt intent rolls forward unless it was voided or superseded by a later attempt. Absence from the database never voids an intent, because a rollback looks identical. Only a worker that observed a definite rollback appends a void. An ambiguous commit is in doubt.
6. **Commit-first events need a current sequence.** If the database's sequence is behind the journal, or the process has not verified or cannot reach the journal, the event goes to a separate intake. Intake entries take no sequence, are effective immediately, and are merged by reconciliation.
7. **Recovery check before dispatch.** Generation matches, quarantine is off, the database is not behind any confirmed sequence or heartbeat, the journal is not shorter than the acknowledged length, and nothing needs reconciliation.

## Crash windows

| Crash point | State left behind | Outcome |
| --- | --- | --- |
| Before the intent is appended | Nothing | Retry |
| After intent, before commit | In-doubt intent | Rolls forward |
| During commit, outcome unknown | In-doubt intent | Rolls forward; idempotent if it had committed |
| After commit, before confirmation | Erased, unconfirmed | Reconciliation confirms |
| Journal-first hold or acceptance, before commit | In-doubt entry | Applied. Holds err toward retaining |
| Commit-first event, before append | Event queued in the database | Flushed idempotently |
| Restore: after load | Quarantined copy | Command resumes |
| Restore: after cutover | Old writers fenced | Command resumes |
| Restore: during apply | Atomic per apply | Command resumes |

## Journal availability

| Operation | Journal unavailable |
| --- | --- |
| Withdrawal, objection, denial, unlink, revocation | Accepted. Goes to intake or queues for the journal |
| Erasure acceptance or intent, hold, restriction, fence clear | Not started, reported as not applied |
| Dispatch, purges | Refused |
| Reads | Continue within the configured staleness bound, then stop |

## Restore

1. Load into a quarantined target. Old writers cannot commit to it.
2. Advance the generation by compare-and-set at cutover. Rehearsals skip this and have no external effects.
3. Run pending migrations under the restore credential.
4. Replay the journal, merge intake, advance the sequence past the journal's maximum, assign fresh sequences to surviving unjournaled events, and clear quarantine in one transaction.
5. Consent-based activation, erasure and purges stay closed until a controller records that the unjournaled window is reconciled.

**Journal loss.** A re-baseline starts a new journal from a database verified as current, with maintainer approval and a recorded reason. Backups older than the re-baseline are no longer restorable under this protocol and must be retired.

## Concurrent erasure

Scope locks serialize attempts. A retry after a crash reconciles first, so at most one erasure takes effect. A retry supersedes an earlier unresolved intent.

## Evidence

`tools/verification/recovery_protocol_model.py` enumerates every interleaving within small budgets. It covers two workers, crashes at each step, journal outages and tail loss, declared and undeclared rollbacks, zombie workers, and intake. It explored about 2.9 million states across nine scenarios with no violation. Eleven mutants, each removing one rule, were all caught.

The model found three defects in the earlier design: a surviving event could reuse a sequence after restore, a replayed event could duplicate a journal entry, and a commit-first event after an undeclared rollback could reuse a sequence.

**This is a design check, not proof of an implementation.** It models one scope with bounded operations and abstracts PostgreSQL, the network and storage. It says nothing about real durability or latency. [#27](https://github.com/buelbuel/customer-fabric/issues/27) and [#85](https://github.com/buelbuel/customer-fabric/issues/85) must reproduce each crash window against real PostgreSQL and a conformance-tested backend.

## Residual risks

- **Undeclared rollback** (for example a volume snapshot restored outside the restore command) can lose a commit-first event that had not reached the journal. Nothing can detect that. The model keeps a witness of this case. Exposure is bounded by flush and heartbeat frequency.
- **Double fault:** a restored database together with a truncated journal cannot be detected from the database. Retention locks reduce this.
- **In-doubt intents always roll forward**, so an erasure that never committed may still be executed. It was already accepted and passed its hold check, and a controller can still refuse before it starts.
- An administrator of a PostgreSQL backend can alter entries. The hash chain reveals it only if the head is anchored elsewhere.
