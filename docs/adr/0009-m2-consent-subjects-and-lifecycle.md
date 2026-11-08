# ADR 0009: M2 controller, consent subjects and data lifecycle

Status: **proposed, 2026-11-08**. Implementation of [#22](https://github.com/buelbuel/customer-fabric/issues/22) and later M2 work waits for maintainer review of this ADR. Refines [ADR 0003](0003-data-lifecycle.md) and builds on ADRs [0004](0004-authentication-boundaries.md), [0005](0005-customer-profile-linking.md), [0006](0006-browser-customer-lifecycle.md) and [0008](0008-operator-identity.md). It amends [ADR 0007](0007-verified-enterprise-associations.md) for the subject export path in section 11, and [ADR 0008](0008-operator-identity.md) by adding the audited `lifecycle_admin` role in section 9. The text of accepted ADRs stays as written.

## Context

M1 ends with four facts that M2 has to design around:

- A customer's Customer Fabric profile is an empty anchor. It has no creator source and no write path, so its attributes stay `{}`.
- Source data lives on other profiles, reached through `verified_associations.record_profile_id`. One source profile can hold several identifiers, and each can be linked to a different subject.
- `profiles` has no lifecycle state. Mutation receipts are the only replay protection, and deleting one lets its idempotency key execute again.
- `record_profile_id`, `record_epoch` and `linking_grants.customer_subject` have no foreign keys. `verified_associations.customer_subject` and `grant_id` do. A missed erasure path through an unconstrained column raises no error.

Everything here is a technical model. Lawful-basis choices, retention periods and exceptions are decisions for each controller and need legal validation before real data. Nothing in this ADR establishes GDPR compliance.

## Decisions

### 1. One configured controller per deployment

A dedicated deployment serves exactly one controller. A singleton `controller` row stores a stable UUID and display reference. `CF_CONTROLLER_ID` must match it at startup and a mismatch fails closed. This is a configuration sanity check, not a backup control. Changing the legal name keeps the UUID. Changing the controller entity needs a new ADR.

Only `purposes`, `policy_versions` and consent evidence reference the controller. There is no `tenant_id`, no per-request controller selection and no multi-controller behavior.

### 2. Entities and their relationships

| Entity | What it is | Relationship and limits |
| --- | --- | --- |
| Kratos identity | Authentication subject in one trusted issuer | Proves a session and control of the current email. Grants no consent and owns no data. |
| Customer anchor | The profile from `customer_identity_links` | One per Kratos subject. Stable across email changes and recovery. Survives loss of the Kratos identity until erasure completes. |
| Source record | One `external_identifiers` binding, identified by `record_epoch` | Belongs to a source and sits on a source profile. A recreated identifier has a new epoch and inherits nothing. |
| Verified association | Proof-backed link between a Kratos subject and one record epoch | Transfers no ownership, merges nothing and implies no consent. |
| Consent subject | The party that a consent decision is about | See section 3. Has its own lifecycle state. |
| Controller | The deployment's accountable organization | Scopes policies and evidence. |
| Purpose | Stable identifier for one processing purpose | Has versioned policies. |
| Policy version | Notice reference, jurisdiction, locale, channels, lawful basis and retention reference | Immutable once active. |
| Evidence | Append-only record of one decision | References one subject and one policy version. |
| Lifecycle | State and epoch of a profile and of a consent subject | Fences writes. Independent of consent. |

### 3. Consent subject

A new `consent_subjects` table gives each subject an opaque UUID, a lifecycle state and exactly one anchor:

- **`customer`**: anchored to the customer anchor profile, not the Kratos UUID, so replacing or deleting an identity does not orphan evidence.
- **`source_record`**: anchored to a `record_epoch`, so a source can report a decision for someone who never registered.

Anchoring source subjects to the record rather than the profile keeps shared source profiles safe: one subject's decision cannot reach another record on the same profile.

Subjects are never merged. Email equality, a matching attribute or a candidate match creates no relationship between them. An active verified association is the only bridge:

- **Grants** made through an authenticated customer session cover the customer subject and records whose association was active when the decision was received (`linked_at <= received_at`), or that the decision names explicitly. Unlinking ends coverage of that record. A grant never reaches a record linked later.
- **Source-reported grants** stay on their record and never raise the customer subject.
- **Withdrawals and objections** are materialized when received: one append-only row for every subject covered at that moment, on either side of an active association. Unlinking never reverses them, so an older grant cannot revive when the bridge goes away.
- **At link time**, the redeem transaction materializes each existing effective withdrawal or objection onto the other side, per purpose, so linking cannot leave a permission standing for someone who already withdrew.

Active associations per subject have a configured maximum, enforced at redemption, which bounds this fan-out.

### 4. Purposes, policy versions and lawful bases

A purpose has one or more policy versions: `draft`, `active` or `retired`. An active version is immutable. Activating a new version never rewrites earlier evidence and never converts it. Each version states whether earlier grants carry forward, so notice edits neither grant consent retroactively nor force blanket re-consent.

Lawful basis is a property of the policy version and is one of consent, contract, legal obligation, vital interests, public task or legitimate interests. It is never inferred and is distinct from permission. Consent decisions apply only to consent-based versions. Legitimate-interest versions record objections instead.

These examples are non-normative. Each controller decides.

| Example purpose | Illustrative basis | Decision model |
| --- | --- | --- |
| Newsletter | Consent | Explicit grant; withdrawal stops it. Per channel. |
| Profiling for personalization | Consent in this reference | A controller may choose otherwise after assessment. Objection handling then applies. |
| Service messages | Contract | No consent decision. Kept separate from marketing purposes. |
| Direct marketing | Consent in this reference | Varies by jurisdiction and channel. Requires controller review. |

### 5. Evidence and effective permission

Evidence is append-only. It records subject, controller, policy version, decision (`grant`, `deny`, `withdraw` or `object`), actor kind, source and source epoch when relevant, `occurred_at`, `received_at`, proof channel, an opaque proof reference and an idempotency key. It holds no raw credentials or contact values. Attribute provenance is never a substitute for it. Writes verify under lock that the subject is live and, for source writes, that the record epoch is live and belongs to the claimed source.

Evaluation ([#24](https://github.com/buelbuel/customer-fabric/issues/24)) lives in a new pure `internal/consent` package with no HTTP or database dependency. The rules that protect a withdrawal are:

- Missing, ambiguous or quarantined evidence fails closed for consent-based purposes. Quarantined `withdraw` or `object` evidence counts as effective.
- Withdrawal and objection apply across every policy version of the purpose, regardless of carry-forward.
- A withdrawal takes effect from `received_at`, so a late or backdated event cannot delay it.
- Until [#35](https://github.com/buelbuel/customer-fabric/issues/35) provides trustworthy ordering, a source-reported grant never supersedes an earlier withdrawal or objection for the same subject and purpose. Only an authenticated customer grant does.

### 6. Lifecycle and minimal epochs in M2

`profiles` and `consent_subjects` gain `lifecycle_state`. `profiles` also gains a monotonic `lifecycle_epoch`, which stamps journal entries and outbox payloads so that [#36](https://github.com/buelbuel/customer-fabric/issues/36) can recheck it at dispatch.

| State | Meaning | Effect |
| --- | --- | --- |
| `active` | Normal | None. |
| `restricted` | Set by an approved restriction ([#30](https://github.com/buelbuel/customer-fabric/issues/30)) | Source writes and activation are rejected. Authorized reads continue. |
| `erasure_pending` | Request accepted and waiting for its identity check or controller review | Same write fence as `restricted`. |
| `erased` | Erasure transaction committed. Terminal | All reads and writes are fenced (section 10). External obligations are tracked separately. |

`sources` gains an `epoch`, advanced by a deployment command run at the same boundary as `migrate`, with an audit row. There is no HTTP endpoint. It survives credential rotation. New M2 source write paths, starting with consent evidence, carry it in `X-Source-Epoch` and receive `409` on an unknown or older value. Existing profile mutations are server-stamped and fenced by section 10.

M2 stops there. Per-record sequences, watermarks, event-time ordering and stale-event quarantine remain in #35, which builds on these columns.

### 7. Referential integrity and the subject-data inventory

| Reference | Decision |
| --- | --- |
| `linking_grants.record_profile_id`, `verified_associations.record_profile_id` | Add foreign keys to `profiles`. Profile rows are never deleted before the section 12 gate, so the target is durable. |
| `linking_grants.record_epoch`, `verified_associations.record_epoch` | No foreign key. Identifier rows are deleted on purpose while history stays; cascading would destroy audit, and `NO ACTION` would block deletion. |
| `linking_grants.customer_subject` | No foreign key. A grant can precede provisioning. |

Foreign keys to a surviving shell cannot detect a missed erasure path, so a reviewed **subject-data inventory** is the guard. It lists every table and column holding a subject, profile or record reference, with a disposition for erasure, export and retention. A test reads the PostgreSQL catalog and fails on an unclassified application table or matching column. After erasure, a verifier in the same transaction counts remaining references across the inventory and rolls back if anything unexpected is left.

### 8. Locking

Every path that touches subject data acquires locks in this one order, skipping classes it does not need:

1. Mutation-key and evidence-idempotency advisory locks
2. Customer subject advisory lock
3. Profile rows, ascending
4. Record-epoch advisory locks, ascending
5. Identifier fingerprint advisory locks, ascending
6. `external_identifiers` rows
7. `consent_subjects` rows, `FOR UPDATE`

`Mutate` follows this order today. Adding fingerprints means taking its class 5 locks, sorted, before the identifier loop. `issueLinkGrant` and `redeemLinkGrant` change: they read the target without locks, then take the customer lock, the anchor and record profile rows in ascending order and the epoch lock, then re-verify. A source-reported withdrawal knows only its record, so it reads the association without locks, takes locks in this order, re-verifies the association and retries if it changed. Erasure discovers its scope the same way, takes all locks, and re-reads the classification. If the profile or epoch set grew, it rolls back and retries a bounded number of times, then reports failure. Deadlocks abort safely and the request retries. No deadlock-free claim is made.

### 9. Erasure across associated source profiles

**Initiation.** A customer requests erasure of their own subject through an authenticated session with a Kratos privileged-session check, separately from any source credential. An operator with a new `lifecycle_admin` role, with a recorded purpose, handles source-record subjects who never registered. The role extends ADR 0008 and is distinct from `profile_reader`. Every request is recorded. Accepting it sets the consent subject, the anchor and each exclusive in-scope record profile to `erasure_pending` until controller policy allows it to run. Shared profiles stay active, and their bindings are handled when erasure runs.

**Attribution.** A profile gains a sticky `attribution_shared` flag, set in the same transaction whenever it first holds a second identifier or a second active association, and backfilled from existing rows. It is never cleared automatically. Exclusivity therefore cannot appear later, after another person's binding is removed. Profiles with several identifiers for one person are also flagged. This is conservative and increases manual review.

Erasure is a controller action, not a source mutation. It records no source provenance and transfers, reassigns or inherits no ownership. Ownership rows for an erased profile are removed under ADR 0003.

| Target | Condition | Action |
| --- | --- | --- |
| Customer anchor | Always | Erase attributes. Keep the shell and link row. |
| Associated record, **exclusive** | Active association and not `attribution_shared` | Erase the profile's attributes, identifiers, ownership and provenance links. Suppress its outbox rows. Keep the shell. |
| Associated record, **shared** | `attribution_shared` | Remove this record binding and association, and fingerprint the identifier. Attributes cannot be attributed to one record, so they stay. Open a review item. |
| Revoked association or deleted record | No live authority | Minimize grant and association metadata. No profile change. |
| Same-email or matching records | No verified association | Never touched. |

The response tells the requester that some records need manual handling. It does not say whether another person is on a profile.

Erasure revokes **all** pending grants by `customer_subject`, not only those for in-scope records. Consent evidence is minimized to what the approved evidence horizon requires, under its own purpose, and the subject enters `erased`.

**Minimizing rows with required columns.** `verified_associations` rows are marked revoked with a new `revoked_by` value, `erasure`. On every `linking_grants` row for the subject, in any state, `secret_hash` is overwritten with random bytes, which satisfies `NOT NULL` and `UNIQUE`, and `customer_subject` is overwritten with a nil UUID. `(source, proof_reference)` stays as the duplicate-proof fence.

**Kratos.** Deletion is external and cannot share the transaction. Commit the fence first, delete the identity through the private admin API, and record the acknowledgment as a durable obligation. After acknowledgment, delete that subject's association rows and then its link row, in that order because of the existing foreign key. The API reports database erasure separately from outstanding obligations. Kratos stores mail recipients and bodies in `courier_messages`; [#27](https://github.com/buelbuel/customer-fabric/issues/27) must verify what identity deletion removes and add the rest to the inventory.

**Journal.** ADR 0003's minimal journal lives in a separate database outside the rollback domain. A write-ahead intent is recorded before commit and reconciled against lifecycle state after a crash. It stores the resolved scope (profile IDs, record epochs, identifier fingerprints and the receipts and grants to re-fence), because scope cannot be re-derived from a restored copy where associations are active again.

### 10. Replay and resurrection fences

| Path | Risk | Fence |
| --- | --- | --- |
| Replay of a kept receipt | Discloses a receipt for an erased subject | On erasure, replace `request_hash` with a sentinel and `result` with a minimal constant. `Mutate` reads lifecycle before comparing the digest, so any replay returns a stable `410` without executing. `erased` is terminal, so that read needs no lock. |
| Replay after a receipt is deleted | `POST /v1/profiles` with the same key creates a new profile | Fenced receipts are kept. Expiry waits for the section 12 gate. |
| `PATCH` to an erased profile | Writes to a shell | Lifecycle check after the row lock returns `410`. |
| New key, same identifier | A fresh write re-attaches an erased identifier | Restricted table of HMAC-keyed fingerprints of `(source, namespace, value)` for every erased binding, including shared ones. A match returns the ordinary identifier conflict, so it reveals nothing about erasure. |
| Customer re-provisioning | `PUT /v1/customer/profile` makes a new blank profile while the Kratos identity is still active | Link row is kept until Kratos deletion is acknowledged. The route returns `410`. |
| Grant redemption or issue | An erased subject acquires an association | All pending grants are revoked. Both paths check the anchor's and record profile's lifecycle under the section 8 locks. |
| Consent evidence replay or re-report | A decision about an erased subject reappears | Evidence keys are fenced like receipts. Writes check subject lifecycle. An erased `source_record` subject stays as a fence row. |
| Reads | Shells appear in results | `Get` and customer reads return `410`. `List` omits erased profiles. |
| Backup restore | Revives erased data | Section 13. |

OpenAPI and tests add these `410` responses in the same change.

Keyed fingerprints are pseudonymous, not anonymous. They need an approved purpose, retention period and rotation plan. The HMAC key lives outside the database rollback domain, and a rotated key is kept until fences written under it expire. A fence is cleared only by an audited `lifecycle_admin` action with a recorded purpose, which starts a new lifecycle identity. ADR 0003's alternative, a fresh enrollment grant, is deferred to M3 connector work. A returning customer who registers again has a new subject and is unaffected. Only re-attachment of the erased source identifier is blocked.

### 11. Export across associated source profiles

Export ([#29](https://github.com/buelbuel/customer-fabric/issues/29)) uses a repeatable-read snapshot and the same scope rules: the customer anchor, then each exclusive active association, presented per source record with provenance. Results are never combined into one merged profile and ownership does not change. Shared, revoked and unlinked records are withheld and reported only as needing manual handling.

This amends ADR 0007, whose association reads give no access to source attributes. That rule still holds for those reads. Export is a separate path that requires a privileged-session check and an issuer attestation that the record concerns a natural person, because ADR 0007 records can be whole accounts. The attestation is recorded on the grant at issuance. Associations without it, including every M1 association, are never exported automatically. The controller must assess access and portability obligations per deployment. Automated scoping reduces the risk of disclosing another person's data but cannot rule it out, because a source can place one person's data on a record linked to another.

### 12. Retention split

Retention work is split so that nothing in M2 depends on M3 ordering:

- **M2, safe now:** a per-category policy framework with no indefinite default, purge receipts and legal-hold checks. It minimizes expired and revoked grants in place as in section 9, keeping `(source, proof_reference)`. It purges revoked association history where no fence depends on it, aged provenance and erased-subject outbox rows. With no dispatcher in M2, other outbox rows have a configured maximum age and are deleted with a purge receipt and metric. Creator authority is never purged.
- **After proven fences ([#86](https://github.com/buelbuel/customer-fabric/issues/86)):** expiry of mutation receipts, fenced receipts, evidence keys, grant proof fences, erased shells and identifier fences. This requires #35's ordering and a demonstration that nothing expired can re-execute.

Purges run in foreign-key order: outbox and provenance, mutations, ownership and identifiers, evidence, consent subjects, associations, grants, link row, profile shell.

### 13. Restore and fault injection in M2

M2 includes a minimal synthetic harness: restore a pre-erasure copy into a disposable, quarantined database with reads, ingestion and dispatch disabled, reapply the journal, and verify that no erased value or actionable receipt remains. If the journal is unavailable, the harness must fail closed. Fault injection covers rollback at every inventory table and concurrent mutation, linking, replay and erasure.

Encrypted backups, WAL policy, key custody, recovery objectives and drills remain production work in [#49](https://github.com/buelbuel/customer-fabric/issues/49).

### 14. Prerequisites for customer-facing consent APIs

Before [#25](https://github.com/buelbuel/customer-fabric/issues/25) begins:

- Customer authentication is consolidated into one authenticator used by every customer route. It accepts exactly one of a native session token or the configured browser cookie. Today this logic is duplicated and associations accept native sessions only.
- Cookie-authenticated state changes require the exact configured `Origin`, `Content-Type: application/json` on requests with a body, and reject a present `Sec-Fetch-Site` other than `same-origin`. The same-origin reference proxy must preserve these headers. Native requests must carry no cookie.
- Live OpenAPI validation covers customer, linking and operator endpoints, which today are exercised only by smoke tests.

## Delivery

| Section | Tracked by |
| --- | --- |
| 1–5 | [#22](https://github.com/buelbuel/customer-fabric/issues/22), [#23](https://github.com/buelbuel/customer-fabric/issues/23), #24 |
| 6 | [#84](https://github.com/buelbuel/customer-fabric/issues/84), then #35 |
| 7–10 | #27 |
| 11 | #29 |
| 12 | [#28](https://github.com/buelbuel/customer-fabric/issues/28) and #86 |
| 13 | [#85](https://github.com/buelbuel/customer-fabric/issues/85), [#31](https://github.com/buelbuel/customer-fabric/issues/31), and #49 for production |
| 14 | [#87](https://github.com/buelbuel/customer-fabric/issues/87) and [#88](https://github.com/buelbuel/customer-fabric/issues/88) |

## Consequences

Consent and lifecycle add domain packages and tables inside the modular monolith, with no new infrastructure beyond the separate journal database. Shells, fenced receipts, proof fences and fingerprints retain pseudonymous metadata longer than a simple delete would. That is deliberate, and each is bounded by section 12's gate. The sticky attribution flag and shared-profile outcomes create human review work, a real operational cost that avoids erasing another person's record or claiming an erasure that did not happen.

Required evidence before the related work is complete: everything listed in ADR 0003, plus the catalog inventory test failing on an unclassified column; tested foreign-key purge order; withdrawal surviving unlink; withdraw-then-link in both directions; a late source grant failing to override a withdrawal; shared-profile outcomes, including a formerly shared profile staying non-exclusive; re-provisioning during pending Kratos deletion; fenced replay of create, update and evidence; redemption racing erasure; and concurrent erasure against association of the same identifier.
