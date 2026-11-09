# ADR 0010: Recovery journal storage and protocol

Status: **proposed. A second independent review, 2026-11-09, found six critical findings ([review record](../security/recovery-protocol-review-2026-11-09.md)); a bounded v1 redesign now addresses all six ([v1 redesign](../security/recovery-protocol-v1-redesign.md), modelled with ground-truth oracles) and awaits one focused independent review before approval. M2 code stays blocked until this ADR and [ADR 0009](0009-m2-consent-subjects-and-lifecycle.md) are approved.** Refines sections 9 and 13 of ADR 0009. Nothing here is implemented.

## Context

ADR 0009 needs lifecycle facts to survive a restore of the database they protect. The journal and PostgreSQL are separate systems, so **no atomic commit across them exists**. Every rule assumes a crash, timeout or delayed message can occur between any two operations.

## Storage decision

**Accepted with conditions:** independent append-only storage outside the tenant's PostgreSQL recovery domain, behind one interface for self-hosted and Cloud.

Independence solves one problem: a restore rewinding the record of what must be reapplied. An independent store alone orders nothing and fences nothing, so the interface carries those duties.

| Risk | Consequence | Control |
| --- | --- | --- |
| New dependency | An outage stops erasures, holds and dispatch | Availability table. Withdrawals stay available as `pending_journal`. |
| Loss or corruption | Restore protection is gone | Dispatch and erasure stop. Re-baseline procedure. |
| Tail truncation, or a journal restored or failed over to an older state | Acknowledged facts disappear | The database stores the head position and hash it acknowledged. A mismatch stops dispatch. |
| Database and journal both rewound | Neither can detect the other | An externally anchored head. See failure guarantees. |
| Two timelines writing | Abandoned processes corrupt the record | Expected-position append, with generation changes recorded in the log. |
| Retention | The journal holds pseudonymous personal data and cannot grow forever | Prefix deletion below a horizon: the lowest `applied_through` of the live database and every restorable backup, the oldest unresolved entry and the oldest unreleased hold. A signed checkpoint of position and hash replaces the deleted prefix. The signing key is separate from the delete credential. |
| Cross-tenant access | One credential reads another tenant | Tenant binding comes from the credential, never a parameter. Per-tenant keys. |
| Weak backend | Guarantees silently fail | Conformance suite. Failure-domain claims need operator attestation, because a test cannot prove them. |

**Required interface.** `append(expected_position, entry)` is atomic and fails if the head has moved. It returns a journal-assigned position. Appends are idempotent by event ID: a retry of an event already present returns its position. Also `head()`, `read_since(position)`, `lookup(event_id)`, and `checkpoint()` for the base position. `read_since` below the base fails. An acknowledged append is synchronously durable: on a PostgreSQL backend that means synchronous replication to a quorum, or no automatic failover, so a promotion cannot drop an acknowledged entry.

Generation changes are records in the log, so one conditional operation covers both order and generation. On object storage this means dense slots written with `If-None-Match`. Appends serialize per tenant, which suits this low-volume stream. Entries carry a schema version, their generation and their full lock set (profiles, record epochs and subject). Replay compatibility is tested for the whole retention period.

**Eligible backends:** a PostgreSQL instance with independent storage, synchronous durability and no restore except through re-baseline; or object storage with conditional writes and retention locks. Recommended: separate PostgreSQL for self-hosted and development, object storage in a separate account for Cloud. **Not eligible:** a database on the tenant's cluster, the Cloud control plane, Kafka, Temporal, or a file on the database host unless the operator attests an independent failure domain. A PostgreSQL backend enforces append-only by grants, which is weaker against an administrator. Tamper evidence depends on anchoring the head hash outside the backend.

The journal stays in the deployment's approved EU region, including replicas and keys. A region failure counts as journal unavailability.

## v1 conservative erasure

The six 2026-11-09 criticals shared one cause: automating erasure of records whose protection depends on facts a rollback or restore can lose. The **v1 design removes that cause** rather than adding mechanisms, and is the recommended scope for the first release. Full counterexamples, acceptance criteria and the ground-truth model are in the [v1 redesign](../security/recovery-protocol-v1-redesign.md) (`tools/verification/recovery_protocol_v1_model.py`).

- **Only proven-separable subscopes are automatic.** A subscope is auto only with separation proven under lock, a visible `erasure_pending` fence, and no rollback since the intent. Every shared, unattested, ambiguous or held subscope is **manual**: restricted and unchanged until a controller records `authorize(intent, record)` (C1, C2).
- **After a restore, auto-classification stays closed** until a controller attests source re-synchronisation, which re-reads the source of truth. Erasure fails closed unless the recovery check passes (C1).
- **Subject-level changes respect record protections**: a row referencing a protected record is untouched without that record's authorization (C3).
- **A stopped subscope is a durable journal obligation.** Retention pins it and its hold until an authorization or terminal disposition; hold release is confirmed and resumes nothing (C4).
- **Kratos deletion waits** until no open subscope of the subject remains (C5).
- **Witness unavailability fails closed** wherever a `witnessed` acknowledgment was issued (C6).

The rest of this ADR is the broader protocol the review examined; where it conflicts with the six rules above, v1 governs. The ambiguous-erasure automation it describes is deferred: for v1 it is a manual, authorization-gated workflow.

## Erasure subscopes

A legal hold for one subject does not automatically stop an erasure of another subject's independent records. An erasure is split into **subscopes**, one per record binding plus one for the subject itself, and each subscope has its own outcome.

**Separation must be proven.** A subscope is `independent` only if, under the scope locks, it passes the exclusive test in ADR 0009 section 9 (active association, natural-person attestation, not `attribution_shared`) and its locks intersect no unreleased hold. Everything else is `protected`: shared records, unattested records, a record whose classification cannot be read, and anything under a hold. A protected subscope is not changed. It gets the restriction flag and a review item.

The intent records each subscope's classification and lists protected subscopes separately. Classification only moves toward protected. Before executing, a subscope is re-tested against the database and the journal, so a profile that became shared after the intent stays untouched. No automated path turns `protected` into `independent`.

| Hold or refusal | Effect on another subject's erasure |
| --- | --- |
| Hold whose locks intersect the subscope | That subscope is protected until reviewed |
| Hold on a different profile, record or subject | None. Independent subscopes proceed |
| Refusal or newer intent | Matches by subject and acceptance ID. Never reaches another subject |

A held subject cannot gain a new association until the hold is released. The subject enters `erased` only when no subscope is still protected. Until then it stays `erasure_pending`, withdrawals stay effective, and the link row and Kratos identity are kept because the review needs them.

**Releasing a hold resumes nothing.** A subscope stopped by a hold stays stopped for that intent, permanently. To continue, a controller records an authorization decision, which starts a new journal-first intent from a fresh scope evaluation with a new lock set and classification. The decision names the reviewer, basis and prior intent. A plain erasure request cannot include a subscope a hold has stopped.

## Protocol

A **scope** is the profiles, records and consent subject locked under ADR 0009 section 8.

1. **Ordering.**

   | Event | Order |
   | --- | --- |
   | Erasure acceptance, refusal, intent and roll-forward claim; holds and hold release; restrictions; objection withdrawal; fence clears; authorization decisions | Journal first, then commit |
   | Withdrawals, denials, objections, unlinks, revocations, loss of separation | Commit first, then journal |
   | Grants | Not journaled, so a restore can only lose permission |

2. **Position and rows.** The journal assigns order. The database keeps three singleton rows. The **generation row** holds lineage UUID and generation, and only the restore command writes it. The **progress row** holds the acknowledged position and hash, and is updated last, immediately before commit, with no external I/O after it. The **heartbeat row** holds the last committed heartbeat position. `applied_through` is a dense low watermark advanced only by the single sweeper, one position at a time and only past resolved entries, so it stays correct under out-of-order commits. Fenced transactions use `synchronous_commit=on`.
3. **Transaction fence.** Every fenced transaction takes `FOR KEY SHARE` on the generation row first, and verifies generation. ADR 0009 section 8 places this before every other lock class. `FOR KEY SHARE` conflicts only with `FOR UPDATE`, so the restore command takes `FOR UPDATE`. A non-key `UPDATE` would not wait.
4. **Journal-first append.** Under the scope locks: `read_since` the writer's last verified position, abort on any generation record, then append at the expected position and commit. On a position conflict, rescan from the expected position and abort on an intersecting hold or a same-subject refusal before retrying. Replay discards journal-first entries whose generation is older than the log's generation at their position. On a timeout, read back by event ID before reporting. If unresolved, or if the journal is down during a void, report "unknown", never "not applied". Append timeouts are short, and idle-in-transaction limits stop a dead client holding locks.
5. **Reconcile first.** An acceptance or erasure attempt first takes its own lock set unioned with the full lock set of each unresolved entry that intersects it, in section 8 order, then re-reads the journal. If the set has grown it releases and restarts, a bounded number of times, then reports "retry" and leaves the entry to the sweeper. The union is one level, not transitive: an intent's outcome depends only on holds touching its own subscopes, its own subject's refusals and newer intents, and its own bindings. Scope state is tracked per scope, so one stuck scope does not stop others.
6. **Holds and refusals do not wait for reconciliation.** They take the scope locks with a bounded `lock_timeout`, and on expiry report "not applied, retry". They then append at the next position. An unresolved intent never executes because of them.
7. **Roll forward.** Each subscope of an unresolved intent is decided independently. Under the intent's full lock set the sweeper appends a **claim** at the expected position, then re-tests and executes. The claim moves the head, so a hold that timed out earlier cannot land between decision and execution. A subscope executes unless the intent has been voided, a newer intent or refusal of the same subject exists at a later position, or a hold intersecting it was unreleased at any time after the intent or is unreleased now. Absence from the database never voids an intent, because a rollback looks identical. A **confirmation names the subscopes that executed**, and replay re-erases only those. **Open (C1):** as written this still rolls a subscope forward when the intent's fence was lost to a rollback or restore before it committed, which can erase data whose separation is no longer proven. The review's fix — roll forward only if the `erasure_pending` fence is positively visible, and re-authorize every pending intent after a restore — is not yet applied.
8. **Void.** The worker runs erasure in a savepoint. If the savepoint rolls back, it appends the void while still holding the outer locks. (Open: if the connection dies first, a void can land after a sweeper claim — review finding I6.)
9. **Scope only shrinks.** Before executing, an intent removes association IDs unlinked or revoked since it was written and any subscope that lost separation. Removed items become review items.
10. **Hold release.** Journal first in two entries: `release`, then the commit, then `release_confirm`. A hold counts as released only from its `release_confirm`. A crash in between leaves the hold effective, and replay reapplies it, so an unconfirmed release errs toward retention. Retrying reuses the event ID.
11. **Commit-first events.** Each row carries an event ID and is flushed idempotently, with the same generation check as rule 4. A process that cannot reach the journal, or has not yet verified with an equal generation, writes to a separate insert-only table that takes no scope lock. A process that sees a definite generation mismatch returns an error instead, because its database is known to be abandoned. Effective permission uses `received_at`, so a late-salvaged event cannot reorder decisions. See acknowledgment levels below.
12. **Heartbeat.** One transaction at a time holds the heartbeat row `FOR UPDATE`, reads the last committed position, appends an entry naming it, and commits its own position only if the row still holds the named value. An append that lands late has no transaction behind it and never commits. A rollback ends every open transaction, so no heartbeat crosses one.
13. **Recovery check.** Before acting or dispatching, within a configured staleness: generation and lineage match; the acknowledged position's hash matches the journal or its checkpoint; **no heartbeat after the database's position names a position above it**; the PostgreSQL timeline equals the latest timeline record in the journal; every unmerged witness entry is present in the database; the journal is not behind the external anchor. Older references are harmless, so an abandoned heartbeat cannot stop a healthy deployment. Dispatch also requires the scope to need no reconciliation.

**Timeline.** The reference value is the journal's latest timeline record, never a value stored in the database, which would roll back with it. A lossless failover goes through a promotion command under the restore credential. It proves the new primary holds every acknowledged commit (synchronous standby confirmed, old primary fenced), then appends a timeline record, and service continues without a generation change. A timeline change with no record stops the deployment and needs the restore command.

## Acknowledgment levels

| Level | What is true when returned | Survives a database rollback |
| --- | --- | --- |
| `pending_journal` | Committed with `synchronous_commit=on` to the tenant primary, and in the table every permission check reads | **No.** A PostgreSQL commit alone is never described as rollback-safe |
| `witnessed` | The above, and the event is in the independent witness | Yes, and the recovery check detects its loss |
| `journaled` | The above except the witness: the event is in the journal and the progress row covering it has committed | Yes |

A withdrawal, objection or denial is accepted during a journal or anchor outage only as `pending_journal` or `witnessed`. It must be durably recorded and effective for suppression before the acknowledgment leaves. Permission checks read the primary, or a replica proven to have replayed past that commit.

**Fail closed.** The request returns an error, never an acknowledgment, if the commit cannot be confirmed, the database is isolated or unreachable, the database is known to be abandoned, the journal is behind the anchor, or a suppression read path cannot see the record. A timeout on commit returns "unknown". A witness entry alone is never an acknowledgment, because it does not suppress anything.

### Independent withdrawal witness

**Design.** A put-if-absent store keyed by event ID, in a failure domain separate from both the tenant database and the journal, still in the approved EU region. Each entry holds an opaque event ID, a pseudonymous subject reference, the decision and `received_at`. Order: commit, put, acknowledge. Withdrawals commute and take effect from `received_at`, so the witness needs no sequencing and no conditional head, which is why it can stay available when the journal cannot. Entries merge into the journal idempotently and are deleted once the progress row covers them. It is a data-store register entry and gets conformance tests.

**Evaluation.**

| Option | Result |
| --- | --- |
| PostgreSQL commit only | Available and simple, but not rollback-safe. This is `pending_journal` |
| Local insert-only table | Same recovery domain. Adds no rollback safety |
| Second journal instance | Inherits sequencing and conditional-write dependencies, so it fails with the same class of outage |
| Put-if-absent witness | Survives rollback in the model, detected by the recovery check, applied at restore. Costs one more store, write latency and a register entry. A shared provider outage still drops the level to `pending_journal` |

**Proposed.** Build the witness behind the same conformance approach, and require it before a deployment advertises rollback-safe acknowledgments during a journal outage. A deployment without it reports `pending_journal` and says so. Between a rollback and the next recovery check within the staleness bound, a process may still act on the rolled-back state.

## Crash and timeout windows

| Point | State left | Outcome |
| --- | --- | --- |
| Before intent append | Nothing | Retry |
| Append timed out | Unknown | Read back by ID. A late arrival fails if the head moved. A retry of a landed event is a no-op |
| After intent, before commit | Unresolved intent | Sweeper rolls independent subscopes forward |
| Claim appended, not executed | Unresolved intent | Sweeper retries. A claim decides nothing |
| During commit, outcome unknown | Unresolved intent | Rolls forward; idempotent if it committed |
| After commit, before confirmation | Erased, unconfirmed | Sweeper records confirmation of what executed |
| Savepoint rollback | Void under locks | Retry is a new intent |
| Journal-first hold before commit | Unresolved hold | Applied. Holds err toward retention |
| Release before `release_confirm` | Hold in effect | Retry with the same event ID |
| Commit-first event before flush | Row queued | Flushed idempotently |
| Heartbeat appended, not committed | Unreferenced entry | Next heartbeat names the last committed position. No effect |
| Any restore step | Target isolated | Command resumes |

## Journal availability

| Operation | Journal unavailable |
| --- | --- |
| Withdrawal, objection, denial | `pending_journal`, or `witnessed` when a witness is available. Fails closed as above |
| Unlink, revocation | Accepted and queued |
| Erasure, hold, hold release, restriction, fence clear, authorization | Not started, reported as not applied. A void that cannot be appended reports "unknown" |
| Dispatch, purges | Refused |
| Reads | Continue within the staleness bound, then stop |

A withdrawal cannot be accepted when no database is reachable, including while a restore has the database isolated. The API returns an error so the caller knows it did not take effect. When the anchor is unreachable but the journal is available, withdrawals are `pending_journal` and dispatch, erasure and purges are refused once the anchor check is stale.

## Restore

The restore command is the only writer of generation. It uses a separate credential and is resumable.

1. **Isolate.** Close network access, revoke login for application roles and rotate database and journal credentials. Network rules do not end existing sessions, so terminate every other backend, confirm none remain and take `FOR UPDATE` on the generation row before writing. Stop or revoke writers on the old primary. Refuse a backup whose lineage differs from the journal's.
2. **Salvage.** If the old primary is reachable, read its unjournaled rows and append them.
3. **Load** into the isolated target, running pending migrations.
4. **Cutover.** Append the generation record. Old writers now fail on position.
5. **Apply** in one transaction: confirmed erasures for the subscopes they name, holds, refusals, unlinks, loss of separation, witness entries and other facts, resolved by event ID, and raise the progress row to the journal head. **An unresolved intent does not execute here.** It is recorded as pending, and its scope is restricted. External effects such as Kratos deletion of a confirmed erasure become ledger obligations.
6. **Keep activation closed.** Consent-based activation, erasure and purges stay closed until a controller records a window decision. Its inputs are the last heartbeat time, the cutover time, the salvage result and source re-synchronization status. If salvage was impossible, the decision records an accepted loss. It is not a reconciliation.
7. **After the window decision**, the sweeper re-runs rule 7 on each pending intent against the current journal, including holds and refusals placed during the window.

**Rehearsals** use a read-only journal credential and a distinct deployment binding. Isolation is never lifted and external effects are removed by capability, not by a flag.

**Re-baseline after journal loss.** With maintainer approval and a recorded reason, the restore command starts a new journal whose generation continues above the old one. "Current" is an attestation, because nothing can verify it after the loss. The new journal has a new lineage UUID, journal credentials rotate and progress resets. A fresh backup is taken first, then older backups are refused by lineage and retired.

**Detected small loss.** An asynchronous-standby failover that the recovery check detects is repaired through the restore command in place, rather than a full restore.

## Concurrent erasure

Scope locks serialize attempts. A retry reconciles first, so at most one erasure of a subscope takes effect.

## Evidence

Two executable models in `tools/verification`, run by `make check-recovery-protocol` and in CI, and one lock check. **The models are bounded verification, not proof.** They enumerate interleavings within small budgets and say nothing outside them.

- `recovery_protocol_model.py`: one scope, crashes, append timeouts and late arrivals, outages, journal tail loss, declared and undeclared rollback, surviving workers, local intake and salvage. About 1.4 million states across twelve scenarios, with thirteen single-rule mutants caught.
- `recovery_protocol_scopes_model.py`: three subjects in a chain of shared profiles, per-binding subscopes, proven and unproven separation, cross-scope refusals, hold release and authorized resume, roll-forward claims, rollback, restore and the window, an old primary, heartbeats, retention, failover and the witness. About 1.1 million states across 27 scenarios, with 37 mutants caught, each with a printed counterexample. Three known limits are printed as traces. Results are in [the results file](../security/recovery-protocol-model-results.md). **The 2026-11-09 review showed the oracles are weaker than claimed:** they restate the protocol's own lock sets rather than ground truth (so an incomplete hold scope is undetectable), the subject subscope and subject/epoch locks are absent, rule 5's union is not modelled, "replay never extends a partial erasure" has no mutant (a whole-`binds` replay survives), and `journal.prune` deletes nothing so the retention claims are untested. Treat the pass as bounded and partial.
- `generation_lock_check.sh`, on PostgreSQL 17: a non-key `UPDATE` and `FOR NO KEY UPDATE` acquire past `FOR KEY SHARE`, `FOR UPDATE` waits, and terminating sessions clears the way. Run manually, not in CI.

The models found defects that review had not. This round: replay of a confirmed intent would have erased subscopes a hold had withheld, so confirmations now name what executed; a hold that timed out could land between a roll-forward's decision and its execution, hence the claim; and a surviving heartbeat could re-acknowledge past a rollback, hence one heartbeat transaction. Earlier rounds found a stale writer re-reading the head past a hold.

**Not modelled:** lock-order deadlocks (locks are atomic), restart bounds in rule 5, restrictions as a separate flag, migrations, rehearsals, re-baseline, the anchor, the staleness window, commit-first flush from an old primary, the promotion command's proof of no loss, and the real behavior of PostgreSQL beyond the lock check, networks and storage. Treating the recovery check as atomic with the action it guards is a model assumption. The single-scope model uses a transaction counter for heartbeats. [#27](https://github.com/buelbuel/customer-fabric/issues/27) and [#85](https://github.com/buelbuel/customer-fabric/issues/85) must reproduce each window against real PostgreSQL and a conformance-tested backend, including generation races on object storage.

## Failure guarantees

Reduced probability is not treated as safety here. Where nothing can detect a failure, that is stated as no guarantee.

**1. An acknowledged withdrawal after an undeclared database rollback**

| | |
| --- | --- |
| Guaranteed | `journaled` and `witnessed` acknowledgments survive any database rollback. Replay restores them, and a process that runs the recovery check refuses to act until they are applied. Between checks, within the configured staleness, a process may act on the rolled-back state. That window is not guaranteed. |
| Not guaranteed | `pending_journal` is durable only while the database is not rolled back. If it is lost before flushing, and no later heartbeat names a position the rolled-back database lacks, the system does not know. |
| Independent evidence | The witness, for `witnessed` events. Inside the system, a heartbeat naming a position above the database's, or an unrecorded timeline change. Outside it: the caller's receipt with its event ID. The platform's audit of restore operations and the WAL archive are independent only if an operator outside the database's control domain holds them. Apart from the witness check, these reveal a loss afterwards and cannot prevent it. |
| Fail closed on detection | Serving subject data, dispatch, erasure and purges, until the restore command has run and a controller records a window decision. Undetected loss fails nothing closed, so real-data deployments must prohibit undeclared restores by platform control and record that attestation. |

**2. Database and journal both restored to an older state**

| | |
| --- | --- |
| Guaranteed | Nothing from those two stores alone. A `witnessed` event outside the rewound stores is still present. |
| Not guaranteed | Erasures, holds, withdrawals and fences written after the older point are lost, and the two stores agree with each other. |
| Independent evidence | A signed head position and hash written regularly to a third failure domain, under separate credentials. A journal behind that anchor is detectable. Anything after the last anchor write remains undetectable. |
| Fail closed | If the anchor is ahead of the journal: serving, dispatch, erasure, purges and journal writes all stop, and withdrawal requests return an error rather than an acknowledgment. If the anchor is merely unreachable, see journal availability. Real-data deployments require an anchor. Without one this failure is unprotected, and the deployment must say so. |

**3. A hold overriding an unconfirmed erasure**

| | |
| --- | --- |
| Guaranteed | Within the modelled budgets, an unresolved subscope does not execute against a hold intersecting it. A confirmed erasure is never undone by a later hold. A hold whose caller was told "unknown" protects data only once it lands in the journal. Independent subscopes of the same intent are unaffected. |
| Not guaranteed | If an erasure committed, its confirmation never reached the journal, and the database was then rolled back, whether declared or not, a hold leaves that subscope's data present. This cannot be told apart from an erasure that never committed. A later release does not change this, because release resumes nothing. |
| Independent evidence | A Kratos deletion or processor acknowledgment for that intent proves it committed. Without one, and for source subjects who never registered, there is none. |
| Fail closed | The subscope is restricted. Activation is refused, and reads are limited as for any restricted subject (ADR 0009 section 6). Its status stays `unresolved`, is never reported as complete or as not erased, and a review item records the legal decision. |

**4. Late writes from an old primary after restore**

| | |
| --- | --- |
| Guaranteed | A late write cannot enter the new timeline, provided the generation check runs on every append, including commit-first flushes. Journal and database credentials are rotated. If the old primary is reachable, its unjournaled rows are salvaged by event ID before cutover. |
| Not guaranteed | A write acknowledged on an old primary that cannot be reached is lost unless it was `witnessed`, and the acknowledgment cannot be recalled. The window lasts until writers are stopped or a process's staleness bound expires. |
| Independent evidence | The witness, the old primary's WAL if it is later recovered, and caller receipts. Process logs from the abandoned side are not independent. |
| Fail closed | A process that sees a generation mismatch stops acknowledging. Past the staleness bound it stops serving. Writes on a partitioned primary return `pending_journal` or `witnessed`, never `journaled`. Activation, erasure and purges stay closed until a controller records the window decision with the salvage result. A window decision cannot cover writes made after it. |

**5. Separation lost in a rollback**

| | |
| --- | --- |
| Guaranteed | Loss of separation already in the journal or the database stops the subscope. Classification recorded in an intent cannot improve on replay. |
| Not guaranteed | If a profile became shared after an intent, the database rolled back undetected before that fact flushed, and the intent then rolled forward, the subscope executes. The model prints this trace. |
| Independent evidence | The source system's record of the second binding. |
| Fail closed | Only on detection, as in guarantee 1. Real-data deployments rely on the same attestation against undeclared restores. |

## Review findings

**First review (2026-11-08) → rewrite.** The I1–I9 findings below were addressed by this rewrite: restore defers unresolved intents (steps 5–7); one heartbeat transaction with compare-and-swap (rules 12–13); a dense `applied_through` watermark (rule 2); subscopes and refusal matching by subject and acceptance ID; an authorization decision for resume (rule 10); a replay lock set (rules 5, 7); `FOR UPDATE` in restore, checked on real PostgreSQL (rule 3); a promotion command and a journal-held timeline reference; and defined acknowledgment levels.

**Second review (2026-11-09) → not closed.** An independent review of the rewritten text found that the rewrite does **not** meet maintainer decisions 1 and 3 and only partly meets 2, with **six critical findings**. The ADR stays proposed. Full record, traces and fixes: [recovery-protocol-review-2026-11-09.md](../security/recovery-protocol-review-2026-11-09.md).

| ID | Critical finding | Where |
| --- | --- | --- |
| C1 | Roll-forward and the declared restore path erase a subscope whose separation was lost in an unflushed, rolled-back fact | rule 7, restore step 7, guarantee 5 |
| C2 | ADR 0009 §9's action table still orders irreversible changes on shared, unattested and anchor records | ADR 0009 §9 |
| C3 | The subject subscope is undefined; one reading destroys grant and association rows for a held record through a path whose locks exclude it | subscopes, ADR 0009 §9 |
| C4 | Retention can prune the only durable record that a hold stopped a subscope, so a plain erasure resumes it without authorization | storage decision, rule 10 |
| C5 | Restore can raise a Kratos-deletion obligation for a partially executed erasure whose review still needs the identity | restore step 5 |
| C6 | The `witnessed` guarantee is silent on an unreachable witness; dispatch does not fail closed | acknowledgment levels, availability |

Important findings also open: a late old-primary witness write can wedge the restored deployment (I1); the lock-free intake conflicts with ADR 0009 §3/§5 (I3); "withdrawals commute" fails against an authenticated grant under clock skew (I2); timeline equality can pass on a diverged branch (I5); a void can follow an execution (I6); authorization is a bare boolean (I7); explicit review has no execution path (I8); and the model's evidence overstates what it proves (I9). These must be resolved and the model strengthened before approval.

## Other residual risks

- An unresolved subscope may execute even if the intent never committed. It was accepted, classified and hold-checked, and a hold or refusal still stops it.
- An administrator of a PostgreSQL backend can alter entries. The anchor reveals this only for entries older than the last anchor write.
- Subscope splitting relies on the correctness of `attribution_shared` and attestation data. A source that places one person's data on another's exclusive record cannot be detected by this protocol (ADR 0009 section 11).
