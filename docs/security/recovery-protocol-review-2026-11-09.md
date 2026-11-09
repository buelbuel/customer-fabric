# Independent adversarial review of ADR 0010 — 2026-11-09

Independent review of commit `98c09a6` (ADR 0010 and ADR 0009 after the I1–I9 rewrite), run by a reviewer that did not write the design. It is reproduced here so the maintainer has it with the PR. Experiments used copied, not modified, model files.

**VERDICT: 6 critical design findings.** ADR 0009 and ADR 0010 stay proposed.

## Against the maintainer's decisions

- **Decision 1 (holds split into safe subscopes; never erase data whose separation cannot be proven): NOT met.** C1, C2, C3.
- **Decision 2 (withdrawals during an outage; no rollback-safe claim from a PostgreSQL commit; independent durable intake; fail closed): partly met.** `pending_journal` is honest and fails closed; the `witnessed` guarantee has a hole (C6) and the lock-free intake conflicts with ADR 0009 (I3).
- **Decision 3 (release does not auto-resume; fresh evaluation and recorded authorization): NOT met as written.** Retention can delete the only stated record of the stop (C4).

Tags: [DEMONSTRATED] = printed model trace; [REASONED] = text analysis only.

## Critical

- **C1. Roll-forward and the declared restore path erase a subscope whose separation was lost in an unflushed, rolled-back fact.** [DEMONSTRATED] An intent is appended, the worker crashes before the fence commits, the profile then becomes shared or the association is revoked, a rollback or restore loses that unflushed fact, the window is accepted, and the sweeper erases what is now another person's data. A window decision accepts lost writes; it does not prove separation. Traces: declared restore 12,998 states; undeclared rollback 2,439 states (one added oracle). *Fix:* roll a subscope forward only if the intent's `erasure_pending` fence is positively visible in the database; after any restore, re-evaluate and re-authorize every pending intent; otherwise protect and review.
- **C2. ADR 0009 §9's action table still orders irreversible changes on shared, unattested and anchor records.** [REASONED] The "shared", "unattested" and "customer anchor (Always)" rows contradict "protected subscopes are unchanged, restricted and sent to review". An implementer following the table performs an irreversible change on an unproven record, and on the anchor under a subject hold; `erased` is terminal. *Fix:* rewrite the table so shared/unattested rows do only restriction and review, and make the anchor row conditional on its subscope being independent.
- **C3. The subject subscope is undefined, and the reading in which it runs destroys held data.** [REASONED] The independence test needs an active association and attestation, which the subject itself and a never-registered source subject cannot pass. One reading freezes every subject forever; the other lets subject-level work (revoke all pending grants by `customer_subject`, overwrite `secret_hash`/`customer_subject`, minimize evidence and association metadata) run through a path whose locks exclude a held shared profile, destroying the grant and association rows tying the subject to that held record. *Fix:* define the subject subscope's contents and test, and give each row it touches the lock of the record it refers to; rows referring to a protected record stay untouched.
- **C4. Retention deletes the only stated record that a hold stopped a subscope, so a plain erasure resumes it without authorization.** [DEMONSTRATED, patched copy] The horizon treats a hold-stopped intent as resolved and its hold as released, so both can be pruned; "a plain erasure request cannot include a subscope a hold has stopped" names no durable source other than the journal. Trace 2,870 states. *Fix:* a stopped subscope stays an unresolved, pinned entry until an authorization decision or a recorded terminal disposition.
- **C5. Restore step 5 can delete the Kratos identity for a partially executed erasure whose review still needs it.** [REASONED] Rule 7 makes partial confirmations normal, but step 5 turns a confirmed subscope's Kratos deletion into an obligation while the subscope section says the identity is kept for review. *Fix:* raise the deletion obligation only when the journal shows no protected subscope remains for that subject.
- **C6. The `witnessed` guarantee does not hold when the witness cannot be read.** [DEMONSTRATED, patched copy] The ADR never says what the recovery check does if the witness is unreachable, and the availability table fails closed only for a journal outage. Trace 1,175 states. *Fix:* a recovery check that cannot read the witness fails closed for dispatch, erasure and purges wherever `witnessed` is issued; add it to the availability table and model it.

## Important

- **I1.** A late old-primary witness write (entries carry no generation; step 1 rotates only DB and journal credentials) can wedge the restored deployment: rule 13 fails and the only stated applier of witness entries is a restore. *Fix:* lineage/generation on entries, rotate witness credentials, online applier for a late entry. [DEMONSTRATED, extended model]
- **I2.** "Withdrawals commute" is false against an authenticated grant: under clock skew a withdrawal at real T+1 can sort before a grant at T and be lifted. *Fix:* one monotonic per-subject-and-purpose order, or withdrawal-wins within a skew bound. [REASONED]
- **I3.** The lock-free intake (rule 11) contradicts ADR 0009 §3/§5 (verification under lock; materialize on both sides). As-of materialization at merge is unspecified, and rule 11 also lists unlinks/revocations/loss-of-separation without saying they still take the `attribution_shared` profile lock. *Fix:* restrict the table to withdrawal-class events and define as-of merge. [REASONED]
- **I4.** "A held subject cannot gain a new association" has no enforcement: redemption checks lifecycle only, not the journal, and a hold is not a lifecycle state. *Fix:* redemption checks holds including unresolved entries; define hold scope to include revoked history. [REASONED]
- **I5.** Timeline equality can pass on a diverged branch (PostgreSQL timeline IDs are not unique across branches). *Fix:* record system identifier, timeline and switch LSN and compare history; state crash-between-promotion-and-append resumption. [REASONED]
- **I6.** A void can follow an execution if the worker's connection dies after savepoint rollback and the sweeper claims/executes/confirms first. *Fix:* reject a void once a later claim exists; a confirmation outranks a void. [REASONED]
- **I7.** Authorization is modelled only as a boolean; nothing makes a decision single-use or binds it to its intent. [REASONED]
- **I8.** Explicit review decisions ("erase by attribute" etc.) have no journal entry type and no execution path; a fresh evaluation reclassifies the record as protected. [REASONED]
- **I9. The evidence is stronger in the text than in the code.** "Replay never extends a partial erasure" has no mutant — a whole-`binds` replay SURVIVED all 27 scenarios; only the withheld-bindings variant is caught. The witness "applied at restore" line, when deleted, still passes. "Judged by oracles the mutant cannot change" is false: `violations` reads `cfg` and the mutated `verified`/`can_dispatch`. The oracles restate the protocol's own lock sets, so an incomplete hold scope cannot be detected; only the limit oracles use ground truth. Rule 5's one-level union is not modelled. "Sweeper records confirmation of what executed" is not modelled, and the ADR does not say how the sweeper learns what executed. [DEMONSTRATED unless noted]

## Minor

Rule 7 matches refusals by subject only vs. subject-and-acceptance-ID in the table; rule 7's "unless" omits loss of separation; refusal missing from the availability table; rule 12's compare-and-swap is dead code under its own premise; restore step 1 takes `FOR UPDATE` before Load on an unstated database and no step writes the DB generation row; `applied_through` location and single-sweeper enforcement unspecified; horizon omits ADR 0009's safety margin; "a replica proven to have replayed past that commit" is not reader-checkable; setting a held exclusive profile to `erasure_pending` conflicts with "not changed".

## What held up

- Baselines reproduced: scopes 1,080,831 states / 27 scenarios / 37 mutants; single-scope 1,411,783 states / 13 mutants; both exit 0, no limit hit.
- Lock check on PostgreSQL 17.11 matches the ADR.
- Larger bounded runs passed without violation (late hold crash=2/timeout=2, 4,291; two restores + two snapshots, 528,802; resume all-2, 168,534). Two large runs hit the 2,000,000-state limit without a violation — **inconclusive**, not clean.
- Rule 7's late-hold race (reasoned), rule 5's one-level claim (reasoned, not tested by the model), and the heartbeat rules (no wedge or false stop found; a rollback within ~one heartbeat interval stays undetectable as stated) held up within what was examined.
