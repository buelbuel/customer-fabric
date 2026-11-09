# Focused independent review of the v1 erasure redesign — 2026-11-09

Independent review of the v1 redesign at commit `b0a40b2`, run by a reviewer that did not write it. Reproduced here so the maintainer has it with the PR. The reviewer did not modify the worktree; experiments ran on copies.

**VERDICT: 1 critical finding — in the verification evidence, not the design. Fixed and re-verified in commit `<this commit>`.**

## What the reviewer confirmed holds

- Baseline reproduced: `recovery_protocol_v1_model.py` exits 0, 10/10 base scenarios OK, 8/8 mutants caught (before the fix). The oracles genuinely read ground truth (`gt_unprovable`, `gt_held`, `ever_held`, `kratos_deleted`, `witnessed`) plus real `authorize` entries — a real improvement over the prior review's I9. The bases are non-vacuous: the only base reaching a silent protected-erasure was the genuinely-undetectable declared limit.
- ADR 0009 §9 table is correctly rewritten: no row orders an automatic irreversible action on a shared, unattested or anchor record; shared/unattested are manual, the anchor is conditional on proven-separable and no hold. C2-in-the-table is closed.
- C3 (subject-level respects record protections), C5 (identity waits for `subject_done`), and C6 (witness outage fails closed) each verified with their mutants.
- The manual-workflow approach for shared/unattested/ambiguous/held erasure is sound and the right v1 call.

## The critical finding (fixed)

The `undeclared and not detectable` exemption was keyed globally and applied to **all** protected records, including `gt_held`/`ever_held`, not only commit-first separation facts. Combined with a retention bug — prune matched a `release_confirm` to a hold by **record** rather than by **instance** — a re-placed, still-active hold could be pruned from the live journal; an undeclared rollback then cleared `db.holds`, and the record was erased with the oracle **silent**.

Demonstrated trace (oracle silent, before fix): `snapshot -> controller.window -> hold.a0 -> release.a0 -> hold.a0 -> undeclared_rollback -> journal.prune -> eraseA.intent -> i1.fence -> i1.execute_auto`, with `a0` under an active legal hold. This breaks invariants #2 and #4 and the evidence did not catch it. A hold is journal-first and rollback-safe, so it is not part of the declared commit-first residual.

## Fix applied and re-verified

1. `separation_only_residual(g, r)`: the exemption now covers a commit-first **separation** loss only, never a `gt_held`/`ever_held` record (three oracle sites).
2. Retention pins each hold instance until a `release_confirm` that follows **it**, so a re-placed hold stays pinned.
3. C3's fault check also covers `ever_held`.
4. New permanent scenario `re-placed hold survives rollback` (32,383 states, OK). Non-vacuity probe: 1,413 states reach "record under an active hold with an open erase intent after an undeclared rollback", zero erase it; reintroducing the old match-by-record bug reproduces the reviewer's exact counterexample, now caught.

Re-run after the fix: 11/11 base scenarios OK, 8/8 mutants caught, TOTAL 84,586 states, exit 0. The reviewer independently validated the exemption-scoping fix ("all 10 bases still OK, all 8 mutants still caught, the held-erase scenario is now caught") with no regression.

## Important points carried as declared limits

- The ghost `detectable` flag and the protocol's `verified` check disagree at the one-heartbeat boundary; where they do, the oracle is conservative (it flags), so no defect is hidden. A rollback within one heartbeat interval is undetectable, as stated.
- INV5 has no exemption; an undeclared rollback reviving an already-erased record after Kratos deletion is covered only by the platform-prohibits-undeclared-restore assumption and should be closed before real data.
- Evidence remains bounded to the six invariants; append races, heartbeat CAS and timeline handling stay in the other two models; real PostgreSQL, networks and storage belong to #27 and #85.
