# Can #22–#24 proceed before the recovery journal?

2026-11-09. Assessment only — **no implementation**. The question: can the consent-purpose registry (#22), the evidence and consent-subject model (#23), and the pure effective-permission evaluation (#24) be built and tested in synthetic development without depending on the unfinished recovery journal (ADR 0010) or irreversible erasure? Erasure, holds, restore and Cloud stay blocked regardless.

## Dependency analysis

| Item | Depends on the journal? | Why |
| --- | --- | --- |
| **#22** purposes, policy versions, lawful bases, controller scoping | **No** | Schema and controller-scoped rows (ADR 0009 §1, §4). The journal exists only to survive a restore of lifecycle facts; registry rows are not lifecycle facts and are not erased. |
| **#23** consent subjects, append-only evidence, withdrawals/denials/objections | **No, with guardrails** | Evidence is append-only (§5); withdrawals are commit-first and explicitly "not blocked by the journal". The journal matters only for erasure and restore, which are out of scope here. |
| **#24** pure `internal/consent` effective-permission evaluation | **No** | A pure function over evidence with no HTTP or DB dependency (§5). It reads committed evidence; it never touches the journal. |

The journal (ADR 0010) and the six erasure invariants protect **erasure and restore**. #22–#24 are the write-up and read path that erasure later fences. Building them first is additive: nothing in #22–#24 must be undone when erasure arrives, provided the guardrails below hold.

## Guardrails that make the separation safe (must be demonstrated, then approved)

1. **No erasure, hold, restriction, restore or purge code** ships with #22–#24. Lifecycle states may exist as columns, but no transition to `erasure_pending`/`erased` and no fence-clearing is implemented.
2. **No rollback-safe acknowledgment is claimed.** Evidence writes acknowledge as `pending_journal` only (ADR 0010 acknowledgment levels). No `witnessed` or `journaled` acknowledgment, because those need the journal and witness that are not built. A synthetic-dev deployment states this.
3. **Evidence stays append-only** and takes the §8 locks it already needs (subject lock, lifecycle check). The generation row (`FOR KEY SHARE`) may be present from `deployment_binding`; it does not require the restore machinery to exist, only the row.
4. **#24 is pure** — no database or journal handle in `internal/consent`. A test asserts the package imports neither.
5. **A boundary test demonstrates the separation**: a build/dependency check that #22–#24 code references no journal, erasure, restore or witness interface. This is the "separation demonstrated" gate the maintainer asked for.
6. **Withdrawal effectiveness is proven within PostgreSQL**: a withdrawal is effective for suppression at evaluation immediately on commit, independent of any journal. The existing ADR 0009 evidence tests (withdraw-then-link both directions, late grant cannot override a withdrawal) cover this.

## What is explicitly still blocked

Irreversible erasure, holds, restore, the journal interface, the witness, Kafka, Temporal, Cloud, and Hydra. Real data additionally needs Kratos #70 and #104 resolved or formally accepted, and legal validation of lawful bases and retention.

## Recommendation

#22–#24 can proceed in **synthetic development** independently of the recovery journal, under guardrails 1–6, because their correctness does not depend on restore protection and they are additive to erasure. Implementation should not begin until (a) the boundary test in guardrail 5 is written and green, demonstrating the separation, and (b) the maintainer approves this scope. This unblocks the useful, reversible part of M2 (consent capture, evidence, permission evaluation) while the irreversible erasure protocol and its ADRs remain proposed.
