# Bounded v1 erasure redesign for C1–C6

2026-11-09. A bounded redesign scoped to the six critical findings of the [2026-11-09 review](recovery-protocol-review-2026-11-09.md) and their direct dependencies. It does not reopen the broader protocol. It is modelled in `tools/verification/recovery_protocol_v1_model.py`, run by `make check-recovery-protocol`. **Bounded verification, not a proof.**

## The architectural choice behind the complexity

The 2026-11-09 criticals share one root: the protocol tried to **automate erasure of records whose protection depends on facts that a rollback or restore can lose** (separation proof, hold state, the record of a stop). Each lost fact needed another mechanism to reconstruct it, and those mechanisms were what the review broke.

v1 removes the cause instead of adding mechanisms. **Only a subscope whose separation is positively proven, fenced and un-rolled-back is automatic. Everything ambiguous is manual:** unchanged until a controller records an authorization. This is the conservative alternative the maintainer asked us to evaluate, and it is the recommendation for v1 (see "Scope recommendation").

## The six invariants and how v1 holds them

Each row gives the finding, a concrete failing counterexample under the old design, the v1 rule, one testable acceptance criterion, and the model mutant/scenario that enforces it. Oracles read **ground truth** in the model's ghost (which record is really shared, really held, which change really touched which record), never the protocol's own classification, so a rule that misclassifies cannot also silence the check.

### C1 — never erase data whose separation is lost, ambiguous or unproven

- **Counterexample (old):** `eraseB.intent → erase1.append → crash → snapshot → share.b1 → restore.apply → window → sweep.claim → sweep.execute` — the fence never committed, the profile became shared, the restore lost that fact, and roll-forward erased it. (Review trace, declared restore, 12,998 states.)
- **v1 rule:** a subscope is auto only if separation is proven under lock, its `erasure_pending` fence is visible, and no rollback/restore has bumped the record's epoch since the intent. Loss of separation is commit-first and effective immediately (sets `attribution_shared` under the profile lock). After any restore, auto-classification stays closed until a controller attests source re-synchronisation (`auto_ok`), which re-reads the source of truth; erasure itself fails closed unless the recovery check passes.
- **Acceptance criterion:** no record that is truly shared/unprovable at erase time is in `erased` without a journaled `authorize`, in any interleaving of crash, share, rollback and restore. (Oracle: `erased protected record without authorization`.)
- **Enforced by:** mutants `C1 classify ignores known-shared`, `C1 roll-forward ignores the fence`, `C2 shared records auto-acted`; scenarios `separation lost then crash`, `separation lost then restore`.

### C2 — protected/shared subscopes stay unchanged until authorized

- **Counterexample (old):** ADR 0009 §9's table ordered "shared → remove binding, fingerprint, subject `erased`" and "unattested → revoke association" as automatic, irreversible actions. (Review C2, reasoned.)
- **v1 rule:** the §9 table's shared, unattested and ambiguous rows do **restriction and a review item only**. No binding removal, fingerprint or `erased` transition happens without a recorded `authorize(intent, record)`.
- **Acceptance criterion:** a shared record is never erased or minimized without an authorization, even after its hold or association changes. (Same oracle as C1.)
- **Enforced by:** mutant `C2 shared records auto-acted`; scenario `shared stays manual`.

### C3 — subject-level changes respect protections on referenced records

- **Counterexample (old):** the subject subscope was undefined; one reading let subject-level work (revoke all grants by `customer_subject`, overwrite `secret_hash`, minimize evidence) destroy the grant and association rows tying the subject to a held shared record, through a path whose locks excluded that record. (Review C3, reasoned.)
- **v1 rule:** subject-level minimization touches a row that references record `r` only when `r` is itself authorized or auto-eligible. Rows referring to a protected record are left untouched. The subject subscope has the same test as a record subscope; a never-registered source subject is always manual.
- **Acceptance criterion:** no subject-level change touches a row referencing a protected record without that record's authorization. (Oracle: `subject-level change touched a protected record without authorization`.)
- **Enforced by:** mutant `C3 subject change ignores protection`; scenarios `hold protects then authorized`, `shared stays manual`.

### C4 — a stopped erasure never resumes after retention or hold release

- **Counterexample (old):** retention pinned "unresolved entries and unreleased holds", but a hold-stopped intent counted as resolved and its hold as released, so both were pruned and a plain erasure resumed the subscope. (Review trace, 2,870 states.) Separately, an unconfirmed release took effect.
- **v1 rule:** a stopped subscope is an **unresolved journal obligation**; retention's horizon pins it and its hold until an authorization or a recorded terminal disposition. Hold release is `release → commit → release_confirm`; it resumes nothing, and a released hold still marks its subscope manual. Resuming needs a fresh `authorize`.
- **Acceptance criterion:** a subscope ever stopped by a hold is never in `erased` without an authorization, across prune and release in any order. (Same oracle as C1.)
- **Enforced by:** mutants `C4 retention prunes a stopped subscope`, `C4 release resumes a stopped subscope`; scenarios `retention with open manual`, `hold release does not resume`.

### C5 — Kratos identity deletion waits for every protected subscope

- **Counterexample (old):** restore step 5 made a confirmed subscope's Kratos deletion an obligation while another subscope was hold-protected and still needed the identity for review. (Review C5, reasoned.)
- **v1 rule:** the identity-deletion obligation is raised only when every in-scope record of the subject is erased — no open auto or manual subscope remains.
- **Acceptance criterion:** `kratos_deleted(subject)` never holds while any of the subject's subscopes is unerased and unauthorized. (Oracle: `deleted the Kratos identity with an open protected subscope`.)
- **Enforced by:** mutant `C5 identity deleted with open manual`; scenario `identity waits for manual`.

### C6 — witness unavailability fails closed

- **Counterexample (old):** the `witnessed` guarantee promised the recovery check refuses to act until witnessed events are applied, but the ADR never said what happens if the witness is unreachable; dispatch proceeded. (Review trace, 1,175 states.)
- **v1 rule:** wherever any `witnessed` acknowledgment was issued, a recovery check that cannot read the witness fails closed for dispatch, erasure and purges; a readable witness holding a withdrawal the database no longer reflects also fails closed.
- **Acceptance criterion:** no dispatch after a witnessed withdrawal is lost, whether the witness is unreadable or shows the loss. (Oracle: `dispatched after losing a witnessed withdrawal`.)
- **Enforced by:** mutant `C6 recovery ignores a witness outage`; scenario `witness outage and rollback`.

## Model summary

```text
BASE   exclusive erasure proceeds: 1202 states, OK
BASE   shared stays manual: 28960 states, OK
BASE   hold protects then authorized: 1368 states, OK
BASE   separation lost then crash: 1412 states, OK
BASE   separation lost then restore: 2893 states, OK
BASE   hold release does not resume: 251 states, OK
BASE   retention with open manual: 251 states, OK
BASE   identity waits for manual: 60 states, OK
BASE   witness outage and rollback: 30 states, OK
BASE   re-placed hold survives rollback: 32383 states, OK
BASE   restore then window: 15776 states, OK
TOTAL  84586 states (bounded verification, not a proof)
MUTANT C1 classify ignores known-shared: caught in 'shared stays manual': C1/C2: erased a record whose separation or hold was lost, without authorization
       trace: controller.window -> eraseA.intent -> i1.fence -> i1.execute_auto
MUTANT C1 roll-forward ignores the fence: caught in 'separation lost then restore': C1/C2: erased a record whose separation or hold was lost, without authorization
       trace: snapshot -> controller.window -> eraseA.intent -> share.a0 -> restore.isolate -> restore.cutover -> restore.apply -> controller.window -> i1.execute_auto
MUTANT C2 shared records auto-acted: caught in 'shared stays manual': C1/C2: erased a record whose separation or hold was lost, without authorization
       trace: controller.window -> eraseA.intent -> i1.fence -> i1.execute_auto
MUTANT C3 subject change ignores protection: caught in 'hold protects then authorized': C3: subject-level change touched a protected record without authorization
       trace: controller.window -> eraseA.intent -> A.subject_minimize
MUTANT C4 retention prunes a stopped subscope: caught in 'retention with open manual': C3: subject-level change touched a protected record without authorization
       trace: controller.window -> eraseA.intent -> hold.a0 -> release.a0 -> journal.prune -> A.subject_minimize
MUTANT C4 release resumes a stopped subscope: caught in 'hold release does not resume': C1/C2: erased a record whose separation or hold was lost, without authorization
       trace: controller.window -> eraseA.intent -> hold.a0 -> release.a0 -> i1.execute_auto
MUTANT C5 identity deleted with open manual: caught in 'identity waits for manual': deleted the Kratos identity of A with an open protected subscope
       trace: controller.window -> eraseA.intent -> A.delete_identity
MUTANT C6 recovery ignores a witness outage: caught in 'witness outage and rollback': dispatched after losing a witnessed withdrawal
       trace: snapshot -> controller.window -> withdraw.witnessed -> undeclared_rollback
```

The `reprove_after_restore` flag (disabling the fence epoch/`auto_ok` check) has no independent counterexample: a detected rollback already fails the recovery check that gates erasure, and an undetected one is the declared residual limit below. It is kept as a flag, not a claimed mutant, to avoid overstating the evidence.

## Focused review and the fix it forced (2026-11-09)

An independent focused review ([review-2 record below]) found one critical gap **in the evidence, not the design**: the `undeclared and not detectable` exemption was keyed globally and so also silenced erasure of a legally-held record, compounded by a retention bug that matched a `release_confirm` to a hold by record rather than by instance — so a re-placed, still-active hold could be pruned and its record erased with the oracle silent. Fixes applied (both validated):

- The exemption is now `separation_only_residual`: it covers a commit-first **separation** loss only, never a `gt_held`/`ever_held` record. A hold is journal-first and rollback-safe, so losing one is never a declared residual.
- Retention pins each hold instance until a `release_confirm` that **follows it**, so a re-placed hold stays pinned.
- A permanent regression scenario `re-placed hold survives rollback` (32,383 states) was added. It is non-vacuous: 1,413 states reach "the record is under an active hold with an open erase intent after an undeclared rollback", and zero erase it; reintroducing the old match-by-record bug reproduces the reviewer's exact counterexample `snapshot -> hold.a0 -> release.a0 -> hold.a0 -> undeclared_rollback -> journal.prune -> eraseA.intent -> i1.fence -> i1.execute_auto`, now caught.

## What is still not guaranteed (declared limits)

- An **undetected undeclared rollback** that loses a **commit-first separation fact** (or a `pending_journal` withdrawal) before the next recovery check, within the staleness bound. This is guarantee 5. It covers separation loss and `pending_journal` withdrawals **only** — never a legal hold. Real-data deployments must prohibit undeclared restores by platform control and attest it; a witness closes the withdrawal case.
- The model's ghost `detectable` flag and the protocol's `verified` check do not model detectability identically at the one-heartbeat boundary: a rollback within one heartbeat interval is undetectable, as stated. Where they disagree the oracle is conservative (it flags), so this does not hide a defect.
- **INV5 has no exemption:** an undeclared rollback that revives an already-erased record after its Kratos deletion is only covered by the platform-prohibits-undeclared-restore assumption; it is not separately enumerated as a residual and should be before real data.
- v1 is bounded to the six invariants. Journal-append races, heartbeat compare-and-swap and timeline handling stay covered by the other two models; real PostgreSQL, networks and storage are out of scope and belong to [#27](https://github.com/buelbuel/customer-fabric/issues/27) and [#85](https://github.com/buelbuel/customer-fabric/issues/85).

## Scope recommendation

For v1, **shared, unattested and ambiguous erasure should be a manual, authorization-gated workflow**, not automated. The model shows the automated path is safe only for proven-exclusive subscopes with a visible fence; everything else reduces to "a controller reviews and records a decision". Automating the ambiguous cases added the state and exceptions that produced C1–C6. Keeping them manual is simpler, is already what the conservative design does, and is the recommended maintainer decision.
