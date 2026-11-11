# ADR 0011: Customer consent API

Status: proposed, 2026-11-09. Implements sections 5, 14 and 16 of proposed ADR 0009 ([PR #101](https://github.com/buelbuel/customer-fabric/pull/101)) for [#25](https://github.com/buelbuel/customer-fabric/issues/25), [#87](https://github.com/buelbuel/customer-fabric/issues/87) and [#88](https://github.com/buelbuel/customer-fabric/issues/88). The number leaves room for ADR 0010 in that PR.

## Decisions

**One customer authenticator.** Every customer route accepts exactly one of a native session token or the configured browser cookie. A native request carries no cookie. Cookie-authenticated writes need the exact browser `Origin`, a `Sec-Fetch-Site` of `same-origin` when present and `application/json` on any body. These checks run before Kratos is called.

**The session is the subject.** No customer consent route accepts a subject, profile, actor, channel or time. The route records `customer` as the actor, the session transport as the proof channel, the Kratos identity as the proof reference and the server clock as `occurred_at`. Precedence still uses the database receipt time. A successful login grants nothing (ADR 0009 section 16).

**Customers can grant and withdraw on consent-based versions.** A grant needs the active version. A withdrawal is also accepted on a retired version, and a purpose stays listed for a customer who decided under its retired consent version so that it remains withdrawable, even after the purpose moves to another lawful basis. A grant is not offered where it could not take effect, such as over an objection. Objections and denials are not offered: M2 has no way for a customer to lift an objection, so offering one would be an irreversible control. Non-consent purposes are listed with no available decision.

**A stale grant is refused; a stale withdrawal never is.** Each purpose has a revision: one plus the customer's own evidence and the record evidence carried to them. A grant quotes it in `If-Match` and is compared while holding the subject, customer identity and linked record locks, in that order, so a grant from an out-of-date view cannot lift a newer withdrawal, including one from an operator or a linked source record still being committed. A retry with the same `Idempotency-Key` returns its original receipt before that comparison and before the version's state is checked.

**The explanation belongs to the policy version.** Decisions are evidence about what a customer was shown, so `explanation` is required on drafts and is immutable once a version is active, like the rest of the notice. No text can stand in for a notice, so the migration fails on a database that already holds policy versions and adds no backfill for this unreleased schema.

**Contracts list each status.** Customer, linking and operator operations document explicit statuses with no `default`, so live validation can fail on an undocumented one. `make m1-contracts` uses real Kratos sessions and is required in CI.

## Consequences

Source consent ingestion is not part of this API; [ADR 0015](0015-source-consent.md) defines it. The customer history shows decisions an operator recorded, but not proof references or source names. Acknowledgments remain `pending_journal`, durable in PostgreSQL but not rollback-safe, until the recovery journal exists. The reference page and API remain development-only and synthetic-data only.
