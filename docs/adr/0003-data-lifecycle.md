# ADR 0003: EU-first data lifecycle and replay safety

Status: accepted design, 2026-11-06. Implementation is deferred; M0 has no subject erasure endpoint or retention worker.

## Context and decision

Current attributes, source identifiers, ownership tombstones, provenance, mutation receipts and outbox rows are retained indefinitely in M0. Only synthetic data is permitted. Metadata and keyed hashes can remain personal data; removing attribute values alone is not erasure. We will implement lifecycle policy inside the modular monolith with PostgreSQL transactions, not a separate event platform.

Controllers must approve a purpose, lawful basis, retention period, trigger, hold rules, processor destinations and accountable owner for every category before real data ingestion. There is no universal GDPR retention duration. Configure shorter periods where possible, with upper bounds and periodic review; never silently fall back to indefinite retention. EU location applies to primary storage, replicas, backups, logs, support access and encryption keys. Region selection alone does not establish compliance. The design follows storage limitation, erasure exceptions, processor obligations and security principles in [GDPR Articles 5, 17, 28 and 32](https://eur-lex.europa.eu/eli/reg/2016/679/2016-05-04/eng/pdf); deployment-specific legal assessment remains necessary.

## Lifecycle and atomic boundary

Use explicit active → restricted/erasure-pending → erased states, with an immutable opaque subject identity and monotonically increasing lifecycle epoch. Authenticate and authorize subject requests separately from source mutation credentials. Verify identity without collecting unnecessary evidence. Scope legal holds to the records and purpose actually required; isolate access, record review/expiry dates, and explain partial refusal without retaining unrelated data.

An erasure transaction locks the profile, increments its epoch, fences ingestion, invalidates actionable receipts, removes current attributes and identifiers, removes or minimizes associated ownership/provenance, suppresses pending deliveries and records a minimal erasure-journal entry. Foreign-key ordering must be explicit and tested. A failure rolls back the entire database change. Later external deletion propagation uses durable outbox work and recorded acknowledgments; the API must distinguish database erasure from pending processor/backup obligations. No completion claim until the defined obligations are satisfied. A dispatcher must check lifecycle state immediately before delivery; consumers must enforce epoch fences because a message already in flight cannot be recalled transactionally.

The deletion journal is distinct from ordinary provenance and is retained under its own restricted purpose and schedule. It contains opaque subject ID, erasure epoch/time, propagation status and policy reference, not erased values. Audit access to it. Customer identity-provider records, exports, caches, search indexes and downstream processors must be explicitly included when introduced. M0 has none of those integrations.

## Category policy

| Category | Required future policy |
| --- | --- |
| Current profile and identifiers | Purpose-bound inactivity/relationship expiry; erase on valid request unless a scoped exception applies. Remove derived lookup indexes too. |
| Provenance | Short, approved audit period; no old/new values. Paths, source names, timestamps and IDs are still sensitive. Minimize or erase subject links when the purpose ends. |
| Ownership tombstones | Preserve ownership only while the profile is active and restoration is authorized. Field removal is not erasure. Delete subject ownership on erasure; use a separate minimal replay fence. |
| Idempotency receipts | Retain only status, opaque IDs and request digest for an approved retry horizon; no historical profile payload. Invalidate on erasure so replay cannot disclose a deleted subject or recreate state. Digests need access controls and eventual expiry. |
| Outbox | Bound pending/retry retention; alert on undeliverable work rather than keeping forever. Payloads remain metadata-only. Erasure supersedes older work; delivery receipts have separate bounded retention. |
| Logs | No credentials, request payloads, identifiers or response bodies. Short operational retention, EU access restrictions and documented incident exceptions. |
| Backups | Fixed expiry, encryption and EU key custody; separate schedules for snapshots, WAL and replicas. Track each recovery point and deletion deadline. |

## Legal holds (implemented, #30)

A legal hold preserves named categories of one scope's data against deletion. It is never global and never open-ended.

- **Scope.** A source record, or a customer. A customer hold covers its profile anchor and the source records verifiably associated with the customer while the hold is in force. It never covers another subject's independent records.
- **Categories.** `profile_data`, `consent_evidence`, `notifications`, `eventing`. A hold preserves only the categories it names; holding one never retains unrelated data.
- **Grounds.** A reason code (`litigation`, `regulatory_investigation`, `statutory_obligation`, `internal_investigation`), a legal reference and a review date at most a year ahead. The legal reference is shown to privacy admins only; audit readers see holds without it.
- **Approval.** A `privacy_admin` requests a hold and a different `privacy_admin` approves it. A request protects while it awaits approval, for at most 14 days, and then lapses.
- **Life.** An approved hold stays in force until released, also past its review date, when it is listed for review (`GET /v1/operator/legal-holds?due=true`). One privacy admin releases a hold, citing a reason; release resumes nothing, and what the hold kept is pruned or erased only by the processes that apply once it is gone. A hold's grounds never change and it moves only forward, enforced in the database; a correction is a new hold. Lists show the newest 200 holds, and 500 due.
- **Audit.** Every request, approval and release is recorded append-only with its actor.
- **Enforcement.** Retention pruning skips what a hold preserves (`notifications`, `eventing`). The retention report shows, per category, how many rows past their age belong to held profiles, an upper bound on what holds keep; it does not count recoveries kept by a hold. `profile_data` and `consent_evidence` can be held already and will bind erasure (#27) when it exists.
- **Not yet.** Holds, like restrictions, are not journaled outside the tenant's recovery domain. That waits for the lifecycle journal (ADR 0009 and ADR 0010, proposed).

## Stale replay and tombstones

Receipt expiry must never become permission to execute an old command again. Before enabling expiry or deletion, introduce source-scoped event IDs, authenticated source epoch, monotonic sequence/watermarks where available, and a bounded accepted event-age window. Reject unknown old epochs, events behind an erasure fence and events outside the accepted window even if the idempotency row has expired. Credential rotation must preserve source identity and replay boundaries. A legitimate new relationship requires explicit authorized creation with a new lifecycle identity; an old source event cannot perform it.

A subject-ID tombstone only fences commands that carry that ID. For identifier-based re-creation, choose and document either a restricted keyed identifier fingerprint denylist or a protocol requiring a fresh authorized enrollment grant. Keyed fingerprints are pseudonymous, vulnerable to authorized dictionary probing, and need purpose, retention and key rotation/deletion policies. Do not call them anonymous or keep them forever. Once suppression data expires, upstream replay must already be impossible under enforced event-age/epoch policy; otherwise erasure cannot safely expire its fence. Retention horizons must cover accepted retries, maximum delivery delay and any restored backup history. Missing trustworthy source ordering requires rejection/quarantine, not guessing based on arrival order.

## Backups and recovery

Immutable backups normally age out on the approved schedule; restricting them is not immediate physical deletion. Explain that distinction in subject responses and processor agreements. Keep the minimal deletion journal outside the same rollback domain as recoverable backups, with independent replication and access controls. Retain it until all backups containing affected data expire plus the tested restore/replay margin.

Restore into quarantine with ingestion, reads and dispatch disabled. Apply all erasure/hold changes newer than the recovery point, restore replay watermarks and rotate compromised credentials if applicable. Validate absence of erased identifiers, payloads and actionable receipts before reopening. Test encrypted backup recovery and key access, expiry of snapshots/WAL, failed journal availability, and downstream replay. If the journal is unavailable, fail closed. Key destruction can support erasure only when key scope and all copies are demonstrably controlled.

## Required acceptance evidence before implementation is complete

Test erasure concurrent with mutation, identifier association, retry and dispatch; rollback on failures at every dependent table; legal-hold partial outcomes; receipt replay before/after expiry; source credential rotation; late messages and old enrollment grants; restore of a pre-erasure backup; journal unavailability; eventual backup expiry; and processor acknowledgment failures. Demonstrate that erased subjects cannot be resurrected and no historical receipt reveals values. Publish measured retention/erasure service levels and operational recovery evidence. This ADR supplies no deletion feature and does not close those production gaps.
