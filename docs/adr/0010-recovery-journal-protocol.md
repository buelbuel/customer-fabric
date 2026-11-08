# ADR 0010: Recovery journal storage and protocol

Status: **proposed, 2026-11-08. M2 code stays blocked until this ADR and [ADR 0009](0009-m2-consent-subjects-and-lifecycle.md) are approved.** Refines sections 9 and 13 of ADR 0009. Nothing here is implemented.

## Context

ADR 0009 needs lifecycle facts to survive a restore of the database they protect. The journal and PostgreSQL are separate systems, so **no atomic commit across them exists**. Every rule assumes a crash, timeout or delayed message can occur between any two operations.

## Storage decision

**Accepted with conditions:** independent append-only storage outside the tenant's PostgreSQL recovery domain, behind one interface for self-hosted and Cloud.

Independence solves one problem: a restore rewinding the record of what must be reapplied. An independent store alone orders nothing and fences nothing, so the interface carries those duties.

| Risk | Consequence | Control |
| --- | --- | --- |
| New dependency | An outage stops erasures, holds and dispatch | Availability table. Withdrawals stay available. |
| Loss or corruption | Restore protection is gone | Dispatch and erasure stop. Re-baseline procedure. |
| Tail truncation, or a journal restored or failed over to an older state | Acknowledged facts disappear | The database stores the head position and hash it acknowledged. A mismatch stops dispatch. |
| Two timelines writing | Abandoned processes corrupt the record | Expected-position append, with generation changes recorded in the log. |
| Retention | The journal holds pseudonymous personal data and cannot grow forever | Prefix deletion past a horizon set from the lowest position any restorable backup has applied and the oldest unresolved entry. A signed checkpoint of position and hash replaces the deleted prefix. The signing key is separate from the delete credential. |
| Cross-tenant access | One credential reads another tenant | Tenant binding comes from the credential, never a parameter. Per-tenant keys. |
| Weak backend | Guarantees silently fail | Conformance suite. Failure-domain claims need operator attestation, because a test cannot prove them. |

**Required interface.** `append(expected_position, entry)` is atomic and fails if the head has moved. It returns a journal-assigned position. Appends are idempotent by event ID. Also `head()`, `read_since(position)`, `lookup(event_id)`, and `checkpoint()` for the base position. `read_since` below the base fails. Acknowledged appends are synchronously durable.

Generation changes are records in the log, so one conditional operation covers both order and generation. On object storage this means dense slots written with `If-None-Match`. Appends serialize per tenant, which suits this low-volume stream. Entries carry a schema version, their generation and their full lock set (profiles, record epochs and subject). Replay compatibility is tested for the whole retention period.

**Eligible backends:** a PostgreSQL instance with independent storage, synchronous durability and no restore except through re-baseline; or object storage with conditional writes and retention locks. Recommended: separate PostgreSQL for self-hosted and development, object storage in a separate account for Cloud. **Not eligible:** a database on the tenant's cluster, the Cloud control plane, Kafka, Temporal, or a file on the database host unless the operator attests an independent failure domain. A PostgreSQL backend enforces append-only by grants, which is weaker against an administrator. Tamper evidence depends on anchoring the head hash outside the backend.

The journal stays in the deployment's approved EU region, including replicas and keys. A region failure counts as journal unavailability.

## Protocol

A **scope** is the profiles, records and consent subject locked under ADR 0009 section 8.

1. **Ordering.**

   | Event | Order |
   | --- | --- |
   | Erasure acceptance, refusal and intent; holds and hold release; restrictions; objection withdrawal; fence clears | Journal first, then commit |
   | Withdrawals, denials, objections, unlinks, revocations | Commit first, then journal |
   | Grants | Not journaled, so a restore can only lose permission |

2. **Position and rows.** The journal assigns order. The database keeps two singleton rows. The **generation row** holds lineage UUID and generation, and only the restore command writes it. The **progress row** holds the acknowledged position and hash and `applied_through`, and is updated last, immediately before commit, with no external I/O after it. Fenced transactions use `synchronous_commit=on`.
3. **Transaction fence.** Every fenced transaction takes `FOR KEY SHARE` on the generation row first, and verifies generation. ADR 0009 section 8 places this before every other lock class.
4. **Journal-first append.** Under the scope locks: `read_since` the writer's last verified position, abort on any generation record, then append at the expected position and commit. Replay discards journal-first entries whose generation is older than the log's generation at their position. On a timeout, read back by event ID before reporting. If unresolved, or if the journal is down during a void, report "unknown", never "not applied". Append timeouts are short, and idle-in-transaction limits stop a dead client holding locks.
5. **Reconcile first.** Acceptance and erasure attempts replay journal entries whose lock sets intersect theirs before acting. A bounded sweeper resolves anything left. Scope state is tracked per scope, so one stuck scope does not stop others.
6. **Holds and refusals do not wait for reconciliation.** They take the scope locks with a bounded `lock_timeout`, and on expiry report "not applied, retry". They then append at the next position. An unresolved intent never executes because of them.
7. **Roll forward.** An unresolved intent executes unless it has been voided, a newer intent exists, a refusal or hold at a later position intersects its lock set, or an unreleased hold at any position intersects it. **An intersecting hold blocks the whole intent** and opens a review item, because erasure is irreversible. Absence from the database never voids an intent, because a rollback looks identical.
8. **Void.** The worker runs erasure in a savepoint. If the savepoint rolls back, it appends the void while still holding the outer locks.
9. **Scope only shrinks.** Before executing, an intent removes association IDs unlinked or revoked since it was written. Removed items become review items.
10. **Commit-first events.** Each row carries an event ID and is flushed idempotently. A process that cannot reach the journal, or has not yet verified with an equal generation, writes to a separate insert-only intake that takes no scope lock. Intake is effective immediately. Merging assigns it to the flush queue. A process that sees a definite generation mismatch returns an error instead, because its database is known to be abandoned. Effective permission uses `received_at`, so a late-salvaged event cannot reorder decisions.
11. **Heartbeat.** The database appends a heartbeat regularly and commits the resulting position to its progress row.
12. **Recovery check.** Before acting or dispatching, within a configured staleness: generation and lineage match, the acknowledged position's hash matches the journal or its checkpoint, the database's acknowledged position is not behind the latest heartbeat by more than one in flight, and the PostgreSQL timeline ID is unchanged. Dispatch also requires the scope to need no reconciliation.

## Crash and timeout windows

| Point | State left | Outcome |
| --- | --- | --- |
| Before intent append | Nothing | Retry |
| Append timed out | Unknown | Read back by ID. A late arrival fails if the head moved |
| After intent, before commit | Unresolved intent | Sweeper rolls forward unless blocked |
| During commit, outcome unknown | Unresolved intent | Rolls forward; idempotent if it committed |
| After commit, before confirmation | Erased, unconfirmed | Sweeper records confirmation |
| Savepoint rollback | Void under locks | Retry is a new intent |
| Journal-first hold before commit | Unresolved hold | Applied. Holds err toward retention |
| Commit-first event before flush | Row queued | Flushed idempotently |
| Any restore step | Target isolated | Command resumes |

## Journal availability

| Operation | Journal unavailable |
| --- | --- |
| Withdrawal, objection, denial | Accepted through intake or the flush queue |
| Unlink, revocation | Accepted and queued |
| Erasure, hold, restriction, fence clear | Not started, reported as not applied. A void that cannot be appended reports "unknown" |
| Dispatch, purges | Refused |
| Reads | Continue within the staleness bound, then stop |

A withdrawal cannot be accepted when no database is reachable, including while a restore has the database isolated. The API returns an error so the caller knows it did not take effect.

## Restore

The restore command is the only writer of generation. It uses a separate credential and is resumable.

1. **Isolate.** Close network access and rotate database and journal credentials before anything can connect. Stop or revoke writers on the old primary. Refuse a backup whose lineage differs from the journal's.
2. **Salvage.** If the old primary is reachable, read its unjournaled and intake rows and append them.
3. **Load** into the isolated target, running pending migrations.
4. **Cutover.** Append the generation record. Old writers now fail on position.
5. **Apply** database changes in one transaction, resolving events by ID and raising the progress row to the journal head. External effects such as Kratos deletion become ledger obligations.
6. **Keep activation closed.** Consent-based activation, erasure and purges stay closed until a controller records a window decision. Its inputs are the last heartbeat time, the cutover time, the salvage result and source re-synchronization status. If salvage was impossible, the decision records an accepted loss. It is not a reconciliation.

**Rehearsals** use a read-only journal credential and a distinct deployment binding. Isolation is never lifted and external effects are removed by capability, not by a flag.

**Re-baseline after journal loss.** With maintainer approval and a recorded reason, the restore command starts a new journal whose generation continues above the old one. "Current" is an attestation, because nothing can verify it after the loss. The new journal has a new lineage UUID, journal credentials rotate and progress resets. A fresh backup is taken first, then older backups are refused by lineage and retired.

**Detected small loss.** An asynchronous-standby failover that the recovery check detects is repaired through the restore command in place, rather than a full restore.

## Concurrent erasure

Scope locks serialize attempts. A retry reconciles first, so at most one erasure takes effect.

## Evidence

`tools/verification/recovery_protocol_model.py` enumerates interleavings within small budgets: two workers on one scope, crashes, append timeouts and late arrivals, outages, journal tail loss, declared and undeclared rollbacks, surviving workers, intake and salvage. It explored about 1.2 million states across twelve scenarios with no violation, and thirteen mutants that each remove one rule were all caught.

**This is a design check, not proof.** It does not yet model overlapping scopes (rules 5 and 7), a writable old primary, unlink scope shrink, restrictions, migrations, rehearsals, prefix deletion, or the real behavior of PostgreSQL, networks and storage. It treats the recovery check as atomic with the action it guards and uses a transaction counter where this ADR specifies heartbeat positions. Some of its invariants have coarse ghost state, which gives false positives just outside the budgets, so those regions are not checked. [#27](https://github.com/buelbuel/customer-fabric/issues/27) and [#85](https://github.com/buelbuel/customer-fabric/issues/85) must reproduce each window against real PostgreSQL and a conformance-tested backend, including racing generation changes on object storage.

## Residual risks

The model keeps executable witnesses of the first two.

- An **undeclared rollback** can lose a commit-first event that had neither reached the journal nor been covered by a heartbeat. Exposure is bounded by flush and heartbeat frequency.
- After an undeclared rollback, a **hold can outrank an unconfirmed erasure**, reviving the subject under hold. This is deliberate: it errs against irreversible erasure of held data.
- A restored database **together with** a truncated journal cannot be detected from the database.
- An unresolved intent may execute even if it never committed. It was accepted and hold-checked, and a hold or refusal still stops it.
- Withdrawals landing on an old primary that cannot be reached after cutover are lost unless salvaged later.
