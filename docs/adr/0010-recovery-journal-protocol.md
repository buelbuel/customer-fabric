# ADR 0010: Recovery journal storage and protocol

Status: **proposed, 2026-11-08. It stays proposed until the remaining critical findings and the overlapping-scope review are resolved, and M2 code stays blocked until this ADR and [ADR 0009](0009-m2-consent-subjects-and-lifecycle.md) are approved.** Refines sections 9 and 13 of ADR 0009. Nothing here is implemented.

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
| Database and journal both rewound | Neither can detect the other | An externally anchored head. See failure guarantees. |
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
4. **Journal-first append.** Under the scope locks: `read_since` the writer's last verified position, abort on any generation record, then append at the expected position and commit. On a position conflict, rescan from the expected position and abort on an intersecting hold or refusal before retrying. Replay discards journal-first entries whose generation is older than the log's generation at their position. On a timeout, read back by event ID before reporting. If unresolved, or if the journal is down during a void, report "unknown", never "not applied". Append timeouts are short, and idle-in-transaction limits stop a dead client holding locks.
5. **Reconcile first.** Acceptance and erasure attempts replay journal entries whose lock sets intersect theirs before acting. A bounded sweeper resolves anything left. Scope state is tracked per scope, so one stuck scope does not stop others.
6. **Holds and refusals do not wait for reconciliation.** They take the scope locks with a bounded `lock_timeout`, and on expiry report "not applied, retry". They then append at the next position. An unresolved intent never executes because of them.
7. **Roll forward.** An unresolved intent executes unless it has been voided, a newer intent exists, a refusal or hold at a later position intersects its lock set, or an unreleased hold at any position intersects it. **An intersecting hold blocks the whole intent**, applies the restriction flag to its scope and opens a review item, because erasure is irreversible. Absence from the database never voids an intent, because a rollback looks identical.
8. **Void.** The worker runs erasure in a savepoint. If the savepoint rolls back, it appends the void while still holding the outer locks.
9. **Scope only shrinks.** Before executing, an intent removes association IDs unlinked or revoked since it was written. Removed items become review items.
10. **Commit-first events.** Each row carries an event ID and is flushed idempotently, with the same generation check as rule 4. A process that cannot reach the journal, or has not yet verified with an equal generation, writes to a separate insert-only intake that takes no scope lock. Intake is effective immediately. Merging assigns it to the flush queue. A process that sees a definite generation mismatch returns an error instead, because its database is known to be abandoned. Effective permission uses `received_at`, so a late-salvaged event cannot reorder decisions. Every acknowledgment states whether it is `journaled` or `pending_journal`, because those carry different guarantees. A flush that completes within a short bound returns `journaled`.
11. **Heartbeat.** The database appends a heartbeat regularly and commits that heartbeat's own position to its progress row, once. It never adopts a later or earlier position, so a rolled-back database cannot re-acknowledge its way past the gap.
12. **Recovery check.** Before acting or dispatching, within a configured staleness: generation and lineage match, the acknowledged position's hash matches the journal or its checkpoint, the database's acknowledged position is not behind the latest heartbeat by more than one in flight, the PostgreSQL timeline ID is unchanged, and the journal is not behind the external anchor. Dispatch also requires the scope to need no reconciliation.

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

Two executable models in `tools/verification`, run by `make check-recovery-protocol` and in CI. **Both are bounded verification, not proof.** They enumerate interleavings within small budgets and say nothing outside them.

- `recovery_protocol_model.py`: one scope, crashes, append timeouts and late arrivals, outages, journal tail loss, declared and undeclared rollback, surviving workers, intake and salvage. About 1.2 million states across twelve scenarios, with thirteen single-rule mutants all caught.
- `recovery_protocol_scopes_model.py`: two erasure scopes intersecting on one profile, with concurrent erasure, legal holds, link and unlink, append races, rollback, restore and a writable old primary. About 640,000 states across eleven scenarios, with fifteen mutants all caught, one of which removes a named pair of rules. Results and counterexample traces are in [the results file](../security/recovery-protocol-model-results.md).

The models found defects that review had not, including a stale writer re-reading the head past a hold and a rolled-back database re-acknowledging a heartbeat. Rule 4's rescan on a position conflict came from the final adversarial review, which showed an erasure could otherwise run past an already journaled hold. Removing the expected-position append or that rescan alone produces a counterexample. The earlier-hold block in rule 7 failed only when reconcile-first was removed too, so within these budgets it is redundant with that rule and is not independently verified.

**Not modelled:** lock-order deadlocks (locks are acquired atomically), more than two scopes in the shipped scenarios, refusals across scopes, hold release, restrictions, migrations, rehearsals, prefix deletion and re-baseline, the anchor, the `applied_through` watermark, the staleness window, commit-first flush from an old primary, partial lock-set replay, and the real behavior of PostgreSQL, networks and storage. Treating the recovery check as atomic with the action it guards is a model assumption, not a property of the design. The single-scope model uses a transaction counter where this ADR specifies heartbeat positions; the two-scope model uses positions. [#27](https://github.com/buelbuel/customer-fabric/issues/27) and [#85](https://github.com/buelbuel/customer-fabric/issues/85) must reproduce each window against real PostgreSQL and a conformance-tested backend, including generation races on object storage.

## Failure guarantees

Reduced probability is not treated as safety here. Where nothing can detect a failure, that is stated as no guarantee.

**1. An acknowledged withdrawal after an undeclared database rollback**

| | |
| --- | --- |
| Guaranteed | An acknowledgment marked `journaled` survives any database rollback. Replay restores it, and a process that runs the recovery check refuses to act until it is applied. Between checks, within the configured staleness, a process may act on the rolled-back state. That window is not guaranteed. |
| Not guaranteed | An acknowledgment marked `pending_journal` is durable only while the database is not rolled back. If it is lost before flushing and before two later heartbeats, the system does not know. |
| Independent evidence | Inside the system, only a heartbeat gap larger than one or a PostgreSQL timeline change. Outside it: the caller's receipt with its event ID. The platform's audit of restore operations and the WAL archive are independent only if an operator outside the database's control domain holds them. All of these reveal a loss afterwards and cannot prevent it. |
| Fail closed on detection | Serving subject data, dispatch, erasure and purges, until the restore command has run and a controller records a window decision. Undetected loss fails nothing closed, so real-data deployments must prohibit undeclared restores by platform control and record that attestation. |

**2. Database and journal both restored to an older state**

| | |
| --- | --- |
| Guaranteed | Nothing from those two stores alone. |
| Not guaranteed | Erasures, holds, withdrawals and fences written after the older point are lost, and the two stores agree with each other. |
| Independent evidence | A signed head position and hash written regularly to a third failure domain, under separate credentials. A journal behind that anchor is detectable. Anything after the last anchor write remains undetectable. |
| Fail closed | If the anchor is ahead of the journal: serving, dispatch, erasure, purges and journal writes all stop, and withdrawal requests return an error rather than an acknowledgment. Behavior when the anchor is merely unreachable is an open maintainer decision. Real-data deployments require an anchor. Without one this failure is unprotected, and the deployment must say so. |

**3. A hold overriding an unconfirmed erasure**

| | |
| --- | --- |
| Guaranteed | Within the modelled budgets, an unresolved intent does not execute against a hold already in the journal or at a later position. A confirmed erasure is never undone by a later hold. A hold whose caller was told "unknown" protects data only once it lands in the journal. |
| Not guaranteed | If an erasure committed, its confirmation never reached the journal, and the database was then rolled back, whether declared or not, a hold leaves the subject's data present. This cannot be told apart from an erasure that never committed. |
| Independent evidence | A Kratos deletion or processor acknowledgment for that intent proves it committed. Without one, and for source subjects who never registered, there is none. |
| Fail closed | The scope is restricted. Activation is refused, and reads are limited as for any restricted subject (ADR 0009 section 6). Erasure status stays `unresolved`, is never reported as complete or as not erased, and a review item records the legal decision. |

**4. Late writes from an old primary after restore**

| | |
| --- | --- |
| Guaranteed | A late write cannot enter the new timeline, provided the generation check runs on every append, including commit-first flushes. Journal and database credentials are rotated. If the old primary is reachable, its unjournaled rows are salvaged by event ID before cutover. |
| Not guaranteed | A write acknowledged on an old primary that cannot be reached is lost, and the acknowledgment cannot be recalled. The window lasts until writers are stopped or a process's staleness bound expires. |
| Independent evidence | The old primary's WAL if it is later recovered, and caller receipts. Process logs from the abandoned side are not independent. |
| Fail closed | A process that sees a generation mismatch stops acknowledging. Past the staleness bound it stops serving. Writes on a partitioned primary return `pending_journal`, never `journaled`. Activation, erasure and purges stay closed until a controller records the window decision with the salvage result. A window decision cannot cover writes made after it. |

## Open findings that block approval

From the final adversarial review, 2026-11-08. These are design gaps with contained fixes, not yet made.

| ID | Finding | Direction |
| --- | --- | --- |
| I1 | Restore step 5 rolls erasures forward before the window decision in step 6 | Record unresolved intents as pending and execute only after the decision, re-running rule 7 |
| I2 | Heartbeat commits are either unsafe without the recovery gate or can wedge with it | One heartbeater; each entry names the last committed position; commit is compare-and-swap |
| I3 | `applied_through` is unsound with out-of-order commits, so the retention horizon can delete needed entries | A dense low watermark advanced only by the sweeper |
| I4 | One subject's refusal or newer intent can block another's intent | Match refusals and newer intents by subject or acceptance ID |
| I5 | An unacknowledged hold release takes effect, erring toward erasure, and blocked intents then run automatically | Release effective only once confirmed; state what happens to blocked intents |
| I6 | Rule 5 does not define the locks replay needs | Transitive union of lock sets, restart if it grows, bounded |
| I7 | `FOR KEY SHARE` does not conflict with a non-key `UPDATE`, and closing network access leaves sessions running | Restore terminates backends and takes `FOR UPDATE` first |
| I8 | Every timeline change stops the deployment, including lossless failover | A no-loss failover path and a defined location for the reference value |
| I9 | `journaled` durability is under-specified for a PostgreSQL backend | Synchronous replication or no failover; commit the progress row before returning `journaled` |

## Other residual risks

- An unresolved intent may execute even if it never committed. It was accepted and hold-checked, and a hold or refusal still stops it.
- An administrator of a PostgreSQL backend can alter entries. The anchor reveals this only for entries older than the last anchor write.
