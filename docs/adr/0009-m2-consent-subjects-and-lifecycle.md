# ADR 0009: M2 controller, consent subjects and data lifecycle

Status: **proposed, 2026-11-08; reconciled the same day with the Cloud, Kafka and Temporal target in [PR #102](https://github.com/buelbuel/customer-fabric/pull/102)**, which is itself a draft. Implementation of [#22](https://github.com/buelbuel/customer-fabric/issues/22) and later M2 work waits for maintainer review of this ADR. Refines [ADR 0003](0003-data-lifecycle.md), including making restriction a flag separate from the lifecycle states, and builds on ADRs [0004](0004-authentication-boundaries.md), [0005](0005-customer-profile-linking.md), [0006](0006-browser-customer-lifecycle.md) and [0008](0008-operator-identity.md). It amends [ADR 0007](0007-verified-enterprise-associations.md) for the subject export path in section 11, and [ADR 0008](0008-operator-identity.md) by adding the audited `lifecycle_admin` role in section 9. The text of accepted ADRs stays as written.

## Context

M1 ends with four facts that M2 has to design around:

- A customer's Customer Fabric profile is an empty anchor. It has no creator source and no write path today, so its attributes stay `{}`. [#75](https://github.com/buelbuel/customer-fabric/issues/75) will add customer-owned attributes.
- Source data lives on other profiles, reached through `verified_associations.record_profile_id`. One source profile can hold several identifiers, and each can be linked to a different subject.
- `profiles` has no lifecycle state. Mutation receipts are the only replay protection, and deleting one lets its idempotency key execute again.
- `record_profile_id`, `record_epoch` and `linking_grants.customer_subject` have no foreign keys. `verified_associations.customer_subject` and `grant_id` do. A missed erasure path through an unconstrained column raises no error.

The product target now also includes Customer Fabric Cloud, Kafka, Temporal and an OIDC capability. **None of them exists in M0 or M1.** This ADR states the privacy contract they must meet. It does not choose topics, schemas, namespaces or an OIDC product.

Everything here is a technical model. Lawful-basis choices, retention periods and exceptions are decisions for each controller and need legal validation before real data. Nothing in this ADR establishes GDPR compliance.

## Decisions

### 1. Deployment boundary and controller

**One configured controller per tenant data plane.** A self-hosted installation is one data plane and needs no Cloud service. Customer Fabric Cloud runs one data plane per paying organization by default. The model is the same in both.

A singleton `deployment_binding` row in each business database stores a stable deployment UUID and the controller's stable UUID and display reference. Only the provisioning or migration step writes it, under a separate credential. The application role can only read it. `CF_DEPLOYMENT_ID` and `CF_CONTROLLER_ID` must match it at startup, and a mismatch or an absent row fails closed, so a process cannot claim an empty database. This catches a process bound to the wrong database. It does not prove that a restored database is current (section 13). Changing the legal name keeps the UUID. Changing the controller entity, or serving several controllers from one data plane, needs a new ADR. A group of companies with separate controllers uses separate data planes.

Deployment and controller are different things: one is a technical boundary, the other a legal entity. Neither is an authorization input a caller can supply.

Only `purposes`, `policy_versions` and consent evidence reference the controller. Every purpose and policy version belongs to one data plane. A provider-supplied template is copied in and versioned independently. There is no `tenant_id` column on business tables, no shared end-customer identity or profile table, no per-request tenant or controller selection, and no cross-tenant query. Tenant identity is established outside the request by immutable deployment routing, credentials, issuer and audience, the database itself, Kafka permissions and Temporal namespace and worker authorization. Each process binds to one data plane at startup.

In Cloud, the service operator is not by default the controller of tenant customer data. Its actual role, and its separate responsibility for staff accounts and billing, must be established in the contracts.

### 2. Entities and their relationships

| Entity | What it is | Relationship and limits |
| --- | --- | --- |
| Tenant data plane | One organization's isolated business database, customer issuer, keys and workers | Self-hosted has exactly one. Everything below except Cloud staff lives inside one. |
| Cloud staff identity | Global staff account in the Cloud control plane | Cloud only. Grants nothing inside a tenant without an active membership and scoped role (section 15). Never a consent subject. |
| Kratos identity | Authentication subject in the data plane's one trusted customer issuer | Proves a session and control of the current email. Grants no consent and owns no data. |
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

A consent subject exists in exactly one data plane. Its UUID is meaningful only there and is never a cross-tenant key. The same person or email in another tenant is an unrelated subject. New kinds, such as anonymous first-party identities ([#77](https://github.com/buelbuel/customer-fabric/issues/77), [#78](https://github.com/buelbuel/customer-fabric/issues/78)), need an amendment that defines their proof and anchor.

Subjects are never merged. Email equality, a matching attribute or a candidate match creates no relationship between them. An active verified association is the only bridge:

- **Grants** made through an authenticated customer session cover the customer subject and records whose association was active when the decision was received (`linked_at <= received_at`), optionally narrowed to those the decision names. Unlinking ends coverage of that record. A grant never reaches a record linked later.
- **Source-reported grants** stay on their record and never raise the customer subject.
- **Withdrawals, denials and objections** are stored once, on the subject they were reported for, and never copied to another subject. Evaluation derives their reach from history: a decision on a source record covers every customer whose association with that record epoch was active when the decision was received, or was created later. It takes effect for that customer from the later of its receipt and the link time.
- **Unlinking** never reverses a decision received while linked, so an older grant cannot revive when the bridge goes away. A decision received after an association ended does not reach that former customer.
- **Linking and relinking** need no write: an existing decision on the record applies from the new link time. A customer grant made before that link therefore cannot lift it; only a later authenticated customer grant can (section 5).

Derivation rests on two persisted facts, both enforced in the database:
- Evidence is append-only, and only the database clock sets its `received_at`.
- Association history is permanent: a `verified_associations` row is never deleted, never moves to another customer or record, and is revoked at most once. Its link and revocation times come from the database clock.

Record decisions take the same record-epoch lock as link, unlink and revoke, so their order is fixed and a concurrent change cannot produce a permissive result. Decisions stay on their record epoch, so a shared source profile spreads nothing to its other people, and a recreated identifier with a new epoch inherits nothing. Evaluation reads this evidence in one snapshot and fails closed when there is more than its bound, or when the subject is not active. Record-subject evaluation does not exist yet; when it is added, it applies the same rule to customer decisions.

This replaces the earlier proposal to copy each decision as rows onto every covered subject at receipt and at link time. Copying needed cross-subject lock ordering, retries and link-time writes in M1 redemption, and a review found it lost decisions under concurrent linking ([#106](https://github.com/buelbuel/customer-fabric/pull/106), [#107](https://github.com/buelbuel/customer-fabric/pull/107)).

### 4. Purposes, policy versions and lawful bases

A purpose has one or more policy versions: `draft`, `active` or `retired`. An active version is immutable. Activating a new version never rewrites earlier evidence and never converts it. Each version states whether earlier grants carry forward, so notice edits neither grant consent retroactively nor force blanket re-consent.

Lawful basis is a property of the policy version and is one of consent, contract, legal obligation, vital interests, public task or legitimate interests. It is never inferred and is distinct from permission. Consent decisions apply only to consent-based versions. Legitimate-interest and public-task versions record objections instead.

These examples are non-normative. Each controller decides.

| Example purpose | Illustrative basis | Decision model |
| --- | --- | --- |
| Newsletter | Consent | Explicit grant; withdrawal stops it. Per channel. |
| Profiling for personalization | Consent in this reference | A controller may choose otherwise after assessment. Objection handling then applies. |
| Service messages | Contract | No consent decision. Kept separate from marketing purposes. |
| Direct marketing | Consent in this reference | Varies by jurisdiction and channel. Requires controller review. |

### 5. Evidence and effective permission

Evidence is append-only. It records subject, controller, policy version, decision (`grant`, `deny`, `withdraw` or `object`), actor kind, source and source epoch when relevant, `occurred_at`, `received_at`, proof channel, an opaque proof reference and an idempotency key. It holds no raw credentials or contact values. Attribute provenance is never a substitute for it. Evidence also records its collection context: the channel and, once OIDC exists, the issuer and client through which the notice was shown. Context explains where a decision was collected and grants nothing (section 16). Writes verify under lock that the subject is not `erased` and, for source writes, that the record epoch is live and belongs to the claimed source. Withdrawals and objections are accepted in every other state, including while restricted or `erasure_pending`, and are never refused because the journal is unavailable. A withdrawal or objection from an authenticated source with a stale epoch is stored as quarantined evidence, which is effective, rather than rejected. Acknowledgments state `pending_journal`, `witnessed` or `journaled` (ADR 0010), which carry different durability guarantees. A PostgreSQL commit alone is never described as rollback-safe. A withdrawal or objection is acknowledged only once it is durably recorded and effective for suppression. If either cannot be guaranteed the request returns an error.

Evaluation ([#24](https://github.com/buelbuel/customer-fabric/issues/24)) lives in a new pure `internal/consent` package with no HTTP or database dependency. The rules that protect a withdrawal are:

- Missing, ambiguous or quarantined evidence fails closed for consent-based purposes. Quarantined `withdraw` or `object` evidence counts as effective.
- Withdrawal, denial and objection apply across every policy version of the purpose, regardless of carry-forward or a change of lawful basis. Changing a purpose's lawful basis needs a recorded controller decision, and processing stays stopped for affected subjects until it exists.
- A withdrawal takes effect from `received_at`, so a late or backdated event cannot delay it.
- Until [#35](https://github.com/buelbuel/customer-fabric/issues/35) provides trustworthy ordering, a source-reported grant never supersedes an earlier denial, withdrawal or objection for the same subject and purpose. Only an authenticated customer grant does.

For one subject and purpose, precedence is:

| Decision | Effect | Lifted by |
| --- | --- | --- |
| `object` | Stops processing on legitimate-interest and public-task versions. A controller may record case-specific compelling grounds to continue, except for direct marketing, where an objection is absolute. | The customer withdrawing the objection |
| `withdraw` | Stops consent-based processing from `received_at` | A later authenticated customer grant |
| `deny` | Same effect as a withdrawal for permission | A later authenticated customer grant |
| `grant` | Permits consent-based processing under its policy version | Any of the above |

The compelling-grounds rule and direct-marketing rule are a proposed reading and need legal validation.

### 6. Lifecycle and minimal epochs in M2

`profiles` and `consent_subjects` gain `lifecycle_state`. `profiles` also gains a monotonic `lifecycle_epoch`, which stamps journal entries and outbox payloads so that [#36](https://github.com/buelbuel/customer-fabric/issues/36) can recheck it at dispatch.

| State | Meaning | Effect |
| --- | --- | --- |
| `active` | Normal | None. |
| `erasure_pending` | Request accepted and waiting for its identity check or controller review, or partly executed with protected subscopes still open (section 9) | Source writes other than withdrawals and objections, grant issue and redemption, and activation are rejected. |
| `erased` | Erasure transaction committed. Terminal | All reads and writes are fenced (section 10). External obligations are tracked separately. |

Allowed transitions are `active` to `erasure_pending`, `erasure_pending` back to `active` when a controller refuses (the refusal is recorded), and `erasure_pending` to `erased`. A consent subject brought into scope by an erasure transaction may go directly from `active` to `erased`.

Restriction ([#30](https://github.com/buelbuel/customer-fabric/issues/30)) is a separate flag, so it survives a pending or refused erasure. While it is set, source writes other than withdrawals and objections, and activation, are rejected, and authorized reads continue.

`sources` gains an `epoch`, advanced per data plane by a deployment command run at the same boundary as `migrate`, with an audit row. Epochs compare as the pair of recovery generation and counter, so a counter rewound by a restore can never repeat an earlier value (section 13). There is no HTTP endpoint. It survives credential rotation. New M2 source write paths, starting with consent evidence, carry it in `X-Source-Epoch` and receive `409` on an unknown or older value, except for the withdrawals and objections in section 5. Existing profile mutations are server-stamped and fenced by section 10.

M2 stops there. Per-record sequences, watermarks, event-time ordering and stale-event quarantine remain in #35, which builds on these columns.

### 7. Referential integrity and the subject-data inventory

| Reference | Decision |
| --- | --- |
| `linking_grants.record_profile_id`, `verified_associations.record_profile_id` | Add foreign keys to `profiles`. Profile rows are never deleted before the section 12 gate, so the target is durable. |
| `linking_grants.record_epoch`, `verified_associations.record_epoch` | No foreign key. Identifier rows are deleted on purpose while history stays; cascading would destroy audit, and `NO ACTION` would block deletion. |
| `linking_grants.customer_subject` | No foreign key. A grant can precede provisioning. |

Foreign keys to a surviving shell cannot detect a missed erasure path, so a reviewed **subject-data inventory** is the guard. It lists every table and column holding a subject, profile or record reference, with a disposition for erasure, export and retention. A test reads the PostgreSQL catalog and fails on any unclassified column of any application table, which also covers references inside `mutations.result` and `outbox.payload`. After erasure, a verifier in the same transaction counts remaining references across the inventory and rolls back if anything unexpected is left.

That inventory covers the business schema and is the same for every deployment. A per-deployment **data-store register** lists everywhere else subject data can exist in that data plane: the customer Kratos database and its mail records, OIDC grant storage, read models, Kafka topics and dead-letter queues, Temporal histories and visibility records, logs, traces, exports, and backups of each. The journal and the fingerprint key store are entries too. Every entry has an owner, a retention bound, an erasure mechanism and a completion signal. Customer Kratos, mail records, logs and backups are mandatory entries in every deployment, including M2. **A store must be registered before it can hold tenant-derived data.** A test checks that every endpoint the deployment configures is registered. It cannot see a store the application does not know about, such as an external log pipeline, so those rely on operator attestation recorded in the register. Cloud control-plane stores have their own separate register.

### 8. Locking

Every path that touches subject data first takes `FOR KEY SHARE` on the generation row (ADR 0010), then acquires locks in this one order, skipping classes it does not need:

1. Mutation-key and evidence-idempotency advisory locks
2. Customer subject advisory lock
3. Profile rows, ascending
4. Record-epoch advisory locks, ascending
5. Identifier fingerprint advisory locks, ascending
6. `external_identifiers` rows, ascending
7. `consent_subjects` rows, ascending, `FOR UPDATE`

`Mutate` follows this order today. Adding fingerprints means taking its class 5 locks, sorted, before the identifier loop. `issueLinkGrant` and `redeemLinkGrant` change: they read the target without locks, then take the customer lock, the anchor and record profile rows in ascending order and the epoch lock, then re-verify. Unlink and issuer revocation also take the profile row before the epoch lock, because they set `attribution_shared`. A source-reported decision takes only its record-epoch lock, the same one link, unlink and revoke take. It writes no row on any other subject, so it needs no customer lock and no retry (section 3). These paths run at READ COMMITTED so that re-verification sees committed changes. Erasure discovers its scope the same way, takes all locks, and re-reads the classification. If the profile or epoch set grew, it rolls back and retries a bounded number of times, then reports failure. Deadlocks abort safely and the request retries. No deadlock-free claim is made.

### 9. Erasure across associated source profiles

**Initiation.** A customer requests erasure of their own subject through an authenticated session with a Kratos privileged-session check, separately from any source credential. An operator with a new `lifecycle_admin` role, with a recorded purpose and identity-verification procedure set by the controller, handles source-record subjects who never registered and customers who have lost their Kratos identity. The role extends ADR 0008 and is distinct from `profile_reader`. In Cloud it is granted per tenant under section 15. Every request is recorded. Accepting it sets the consent subject, the anchor and each exclusive in-scope record profile to `erasure_pending` until controller policy allows it to run. Shared profiles stay active, and their bindings are handled when erasure runs.

**Attribution.** A profile gains a sticky `attribution_shared` flag, set in the same transaction whenever it first holds a second identifier or a second active association, or whenever an association on it is revoked, and backfilled from existing rows. This covers serial holders of one record even after association history is purged, and it also flags a customer who unlinks and relinks. It is never cleared automatically. Exclusivity therefore cannot appear later, after another person's binding is removed. Profiles with several identifiers for one person are also flagged. This is conservative and increases manual review.

Erasure is a controller action, not a source mutation. It records no source provenance and transfers, reassigns or inherits no ownership. Ownership rows for an erased profile are removed under ADR 0003.

Each target is a **subscope** with its own outcome. A subscope is erased automatically only when it is **proven separable** — see [ADR 0010 v1 erasure](0010-recovery-journal-protocol.md#v1-conservative-erasure). Every other subscope is **manual**: restricted, sent to review, and changed only after a controller records an `authorize(intent, record)` decision. **Data whose separation from another person cannot be proven is never erased automatically**, and releasing a hold resumes nothing.

| Target | Condition | Action |
| --- | --- | --- |
| Customer anchor | Its subscope is proven separable and under no hold | Erase attributes. Keep the shell and link row. If the anchor is under a hold or an unresolved subscope needs the identity, it is manual. |
| Associated record, **exclusive** | Active association, natural-person attestation (section 11), not `attribution_shared`, no intersecting hold, fence visible | Erase the profile's attributes, identifiers, ownership and provenance links. Suppress its outbox rows. Keep the shell. The record's consent subject enters `erased`. |
| Associated record, **unattested** | Active association without attestation, including every M1 association | **Manual.** Revoke the association and open a review item. No source attribute is erased and nothing is fingerprinted automatically. The record's consent subject stays `active`, its withdrawals stay effective, and actor references to the erased customer are removed from its evidence. |
| Associated record, **shared** | Attested and `attribution_shared` | **Manual.** Restrict and open a review item. No binding removal, fingerprint or `erased` transition happens without a recorded authorization. Attributes cannot be attributed to one record, so they stay. |
| Source-record subject, never registered | `lifecycle_admin` request, no association, operator-recorded natural-person determination | **Manual.** Has no active association, so it cannot pass the separable test automatically; it is restricted and reviewed, then acted on under a recorded authorization. |
| Revoked association or deleted record | No live authority | Minimize grant and association metadata **for records whose subscope is authorized or proven separable**; a row referencing a protected record is left untouched. No profile change. |
| Same-email or matching records | No verified association | Never touched. |

Subject-level work — revoking pending grants by `customer_subject`, overwriting `secret_hash`, minimizing evidence — touches a row referencing record `r` only when `r`'s subscope is itself authorized or proven separable. The response tells the requester that some records need manual handling. It does not say whether another person is on a profile.

**Shared-profile rights.** Automation never decides whose data a shared attribute is. Each review item ends in a recorded controller decision: release, partial release by attribute, erase by attribute, or refuse with a basis. The record is excluded from activation for the requester while the item is open. Withdrawals and objections stay scoped to the record and are unaffected. Review deadlines are configurable per deployment. `attribution_shared` is cleared only by an audited decision with a second approver.

Open review items are ledger obligations, so overall status cannot become `complete` while one is open.

**Legal holds and subscopes.** An erasure is split into one subscope per record binding plus one for the subject ([ADR 0010](0010-recovery-journal-protocol.md#erasure-subscopes)). A hold for one subject does not automatically stop another subject's independent records. A subscope is independent only if it passes the exclusive test in the table above under lock and intersects no unreleased hold. Shared, unattested or ambiguous records and anything under a hold are protected: unchanged, restricted and sent to review. **Data whose separation from another person cannot be proven is never erased automatically.** The subject enters `erased` only when no subscope is still protected. Until then it stays `erasure_pending`, withdrawals stay effective, and Kratos deletion waits because the review needs the link. A held subject cannot gain a new association until the hold is released.

**Hold release resumes nothing.** A release takes effect only once confirmed in the journal. A subscope a hold stopped stays stopped for that intent. Continuing needs a new erasure from a fresh scope evaluation and an explicit recorded controller authorization.

Erasure revokes **all** pending grants by `customer_subject`, not only those for in-scope records. Consent evidence is minimized to what the approved evidence horizon requires, under its own purpose, and the subject enters `erased`.

**Minimizing rows with required columns.** `verified_associations` rows are marked revoked with a new `revoked_by` value, `erasure`. On every `linking_grants` row for the subject, in any state, `secret_hash` is overwritten with random bytes, which satisfies `NOT NULL` and `UNIQUE`, and `customer_subject` is overwritten with a nil UUID. `(source, proof_reference)` stays as the duplicate-proof fence.

**Kratos.** Deletion is external and cannot share the transaction. Commit the fence first, delete the identity through the private admin API, and record the acknowledgment as a durable obligation. After acknowledgment, delete that subject's association rows and then its link row, in that order because of the existing foreign key. A keyed fingerprint of the erased Kratos subject stays in the business database, so if a rolled-back Kratos revives the identity, `PUT /v1/customer/profile` returns `410` and reopens the deletion obligation instead of provisioning a new profile. The API reports database erasure separately from outstanding obligations, using the status in section 17. The same pattern applies to every other register entry. Kratos stores mail recipients and bodies in `courier_messages`; [#27](https://github.com/buelbuel/customer-fabric/issues/27) must verify what identity deletion removes and add the rest to the inventory.

**Journal.** ADR 0003's minimal journal becomes a **lifecycle journal** outside the recovery domain of the data it protects, so restoring the tenant's databases cannot also rewind it. A second logical database on the same cluster does not qualify where cluster-level recovery could rewind both. It is scoped to one tenant and never lives in the Cloud control plane. It records erasure scope (profile IDs, record epochs, identifier fingerprints, the Kratos subject, receipts and grants to re-fence, and open obligations), plus withdrawals, denials, objections, restrictions, holds, unlinks, grant revocations and fence clears. Scope and state cannot be re-derived from a restored copy where they have rolled back.

[ADR 0010](0010-recovery-journal-protocol.md) defines event ordering, sequences, in-doubt handling and journal availability. In short: erasure, holds and restrictions are journaled first; withdrawals, denials, objections, unlinks and revocations commit first and are not blocked by the journal, only by bounded lock and transaction timeouts; and each subscope of an unresolved erasure intent rolls forward unless an intersecting hold, a same-subject refusal or newer intent, a loss of separation or a void stops it. Application runs the section 7 verifier and stops for review if it fails. An erasure accepted but not yet run returns to `erasure_pending` after a restore. Journal entries are retained until the oldest restorable backup that needs them has expired, plus a safety margin.

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

Keyed fingerprints are pseudonymous, not anonymous. They need an approved purpose, retention period and rotation plan. The HMAC key is unique to each data plane, so one identifier yields unrelated fingerprints in two tenants. It lives outside the database recovery domain, and a rotated key is kept until fences written under it expire. Fence checks test every retained key, and the per-identifier advisory lock does not vary with key generation. New request digests are keyed per data plane too. Receipts on a shared profile keep their digest, so older digests of an erased identifier can remain there. A fence is cleared only by an audited `lifecycle_admin` action with a recorded purpose and a second authorized approver, which starts a new lifecycle identity and is journaled. ADR 0003's alternative, a fresh enrollment grant, is deferred to M3 connector work. A returning customer who registers again has a new subject and is unaffected. Only re-attachment of the erased source identifier is blocked.

### 11. Export across associated source profiles

Export ([#29](https://github.com/buelbuel/customer-fabric/issues/29)) uses a repeatable-read snapshot and the same scope rules: the customer anchor, then each exclusive active association, presented per source record with provenance. Results are never combined into one merged profile and ownership does not change. Shared, revoked and unlinked records are withheld and reported only as needing manual handling.

This amends ADR 0007, whose association reads give no access to source attributes. That rule still holds for those reads. Export is a separate path that requires a privileged-session check and an issuer attestation that the record concerns a natural person, because ADR 0007 records can be whole accounts. The attestation is recorded on the grant at issuance. Associations without it, including every M1 association, are never exported automatically. The controller must assess access and portability obligations per deployment. Automated scoping reduces the risk of disclosing another person's data but cannot rule it out, because a source can place one person's data on a record linked to another.

### 12. Retention split

Retention work is split so that nothing in M2 depends on M3 ordering:

- **M2, safe now:** a per-category policy framework with no indefinite default, purge receipts and legal-hold checks. It minimizes expired and revoked grants in place as in section 9, keeping `(source, proof_reference)`. It purges aged provenance and erased-subject outbox rows. Revoked association history is not purged in M2, because evaluation derives which record decisions cover a customer from it (section 3); purging it later needs the covered evidence to have expired first. With no dispatcher in M2, other outbox rows have a configured maximum age and are deleted with a purge receipt and metric. Creator authority is never purged.
- **After proven fences ([#86](https://github.com/buelbuel/customer-fabric/issues/86)):** expiry of mutation receipts, fenced receipts, evidence keys, grant proof fences, erased shells and identifier fences. This requires #35's ordering and a demonstration that nothing expired can re-execute. For each deployment the horizon must exceed the longest of Kafka retention including dead-letter queues, Temporal retry and history windows, the accepted source-event age, and the restorable backup window.

Purges run in foreign-key order: outbox and provenance, mutations, ownership and identifiers, evidence, consent subjects, associations, grants, link row, profile shell.

### 13. Restore and fault injection in M2

M2 includes a minimal synthetic harness: restore a pre-erasure copy into a disposable, quarantined database with reads, ingestion and dispatch disabled, reapply the journal, and verify that no erased value or actionable receipt remains. If the journal is unavailable, the harness must fail closed. Fault injection covers rollback at every inventory table and concurrent mutation, linking, replay and erasure.

Every restore follows the restore procedure and recovery check in [ADR 0010](0010-recovery-journal-protocol.md), and is enforced rather than procedural:

- Each process runs the recovery check at startup, when a database connection is established and within a configured staleness before acting or dispatching, and refuses on an unreconciled database.
- A dedicated restore command, with a separate credential, is the only writer of generation. Rehearsals advance nothing and have no external effects.
- Applying the journal re-deletes Kratos identities, rebuilds the obligation ledger, and restores withdrawals, denials, objections, restrictions, holds, unlinks, revocations, fence clears and accepted or refused erasures.
- Consent-based activation, erasure and purges stay closed until a controller has recorded a window decision. Unresolved erasure intents wait for it and are then re-evaluated against the current journal.
- The generation is stamped at publication and on durable workflow identities. Consumers and activities reject older generations.

This covers restores carried out through the restore command. An undeclared rollback of a single store is detected only where a fence exists.

Consumer dedupe records roll back with the database, so events retained in Kafka past the recovery point will arrive as if new. Fences, not dedupe, must make that safe. PostgreSQL, Kafka and Temporal share no recovery point: PostgreSQL plus the journal is the authority, and the other stores are assumed to be older or newer.

Restoring one tenant must not affect another. On a shared cluster, physical recovery may not be able to restore one database alone, so per-tenant restore goes through a side instance and a logical copy into quarantine unless the tenant has a dedicated cluster ([#93](https://github.com/buelbuel/customer-fabric/issues/93)).

Encrypted backups, WAL policy, key custody, recovery objectives and drills remain production work in [#49](https://github.com/buelbuel/customer-fabric/issues/49).

### 14. Prerequisites for customer-facing consent APIs

Before [#25](https://github.com/buelbuel/customer-fabric/issues/25) begins:

- Customer authentication is consolidated into one authenticator used by every customer route. It accepts exactly one of a native session token or the configured browser cookie. Today this logic is duplicated and associations accept native sessions only.
- Cookie-authenticated state changes require the exact configured `Origin`, `Content-Type: application/json` on requests with a body, and reject a present `Sec-Fetch-Site` other than `same-origin`. The same-origin reference proxy must preserve these headers. Native requests must carry no cookie. In Cloud, session cookies are host-only for one tenant origin and are never set on a parent domain shared by tenants.
- Live OpenAPI validation covers customer, linking and operator endpoints, which today are exercised only by smoke tests.

### 15. Cloud tenant boundaries

These apply only to Cloud. Self-hosted keeps its local operator issuer and has no control plane.

**Control plane.** It holds organizations, memberships, plans, deployment references and minimal audit. It holds no customer profiles, source records, consent evidence, erasure journals or behavioral events. It orders lifecycle actions by deployment reference and never reads tenant customer data to do so. Deployment references and control-plane audit carry no subject references or tenant-supplied free text.

**Staff authority.** A staff identity is global, and authority is per tenant. A data plane accepts a staff credential only when it comes from the Cloud staff issuer, its audience is that one deployment, and it reflects an active membership and role within a configured maximum staleness. If that cannot be established, for example during a control-plane outage, the request fails closed. No credential covers two tenants. A person who administers tenants A and B holds two separate authorities, and nothing obtained in A has any effect in B. Privileged actions need step-up. Tenant audit records use a per-tenant staff reference so that tenants cannot correlate staff. Membership is never inferred from email. Staff assertions last at most five minutes. Erasure, export and connector-secret changes also need a live membership check, narrowing the "every sensitive action" wording in PR #102. Provider employees have no standing access to tenant customer data. Support access is break-glass only: approved by a tenant administrator, limited to four hours and fully audited. A person who is both staff and a customer of a tenant has two unrelated identities.

**Identity and grant confusion.** The API trusts exactly one customer issuer. Linking issuer credentials, grant secrets, proof references and source names are all scoped to one data plane, so `salesforce` in A is unrelated to `salesforce` in B. A shared connector or proof service holds separate credentials per tenant and cannot present a proof from one tenant to another.

**Shared infrastructure.** Sharing adds requirements that a dedicated deployment would not have:

| Shared component | Additional requirement |
| --- | --- |
| Tenant PostgreSQL cluster | Application roles cannot connect to another database and are never owners or superusers. Connection and resource limits per tenant. Physical backups are a shared-blast-radius store and belong in each tenant's register. |
| Kafka | Per-tenant principals and ACLs with no wildcard across tenants. A tenant identifier in an event envelope is checked against the consumer's bound deployment but is never authority. On a mismatch the receiver drops the value, keeps only the event coordinates and claimed tenant, and raises an incident on the owning side. It never stores or forwards another tenant's payload. |
| Temporal | One namespace per tenant with its own worker credentials. A task-queue name alone is not a boundary. Payload encryption keys are per tenant. |
| Logs, metrics, traces | No customer values or subject references. Tenant labels only. |
| Schema registry | Subjects are separated per tenant so one tenant cannot poison another's schemas. Tenant-defined field names are treated as tenant data. |
| Caches and connection poolers | Keys and pools are per data plane. A pooled connection is never reused across tenants. |
| Mail relays | Hold recipients and bodies, so they are register entries for each tenant. |
| Tenant identifiers, hostnames, issuer URLs | Never reused, so retained topics, namespaces, backups, cookies or tokens cannot be inherited by a later tenant. |

**Termination.** Each step is idempotent, audited and forward-only:

1. Suspend ingress and activation and revoke source and OAuth credentials.
2. Export where lawfully authorized.
3. Stop relays and consumers and drain or cancel workflows, leaving open obligations recorded.
4. Notify processors and record acknowledgments.
5. Delete the tenant's Kafka and Temporal data, noting the retention that still applies.
6. Delete the customer identity, OIDC and business databases.
7. Let backups expire. Keep the journal until the last backup containing the tenant has expired, then destroy the journal and tenant keys.
8. Keep only a minimal closure record in the control plane.

Provisioning failures never delete an existing volume or tenant dataset automatically. Control-plane staff accounts, memberships, audit and billing records have their own retention policy. Removing a staff member removes the mapping behind per-tenant references without rewriting tenant audit history.

### 16. Authentication, OAuth and consent stay separate

These are six separate decisions. None implies another.

| Decision | Authority | Does not grant |
| --- | --- | --- |
| Kratos authentication and verification | Session in the tenant's issuer | Consent, or access to source data |
| OIDC application authorization, scopes and client grants | OAuth grant to a client ([#72](https://github.com/buelbuel/customer-fabric/issues/72), [#74](https://github.com/buelbuel/customer-fabric/issues/74)) | Any processing purpose |
| Processing purpose and lawful basis | Policy version | Permission for an individual |
| Consent evidence and withdrawal | Evidence for one subject | Authentication or API access |
| Reading source-controlled attributes | Separate read policy ([#76](https://github.com/buelbuel/customer-fabric/issues/76)) | Activation |
| Activating data to a destination | Effective permission at dispatch | Anything else |

An OAuth consent screen does not authorize marketing, profiling or unrestricted CDP access. The OIDC `sub` is never a consent key, and a pairwise `sub` maps back only through the customer anchor. Withdrawal does not by itself revoke an application's OAuth grant; it changes what processing is permitted, and every Customer Fabric read and dispatch evaluates that. Erasure does revoke: Kratos sessions, refresh tokens and client grants are register entries. Self-contained tokens outlive revocation until they expire, so token lifetime is part of the erasure bound, and Customer Fabric APIs check lifecycle whatever the token says. External applications holding claims are processors or recipients to be notified. This section sets boundaries only. It does not select Hydra.

### 17. Asynchronous copies and erasure completion

Kafka and Temporal are data stores for privacy purposes. Their logs, dead-letter queues, histories, visibility records and backups are register entries.

**Erasure status.** Erasure is reported per register entry as `pending`, `fenced`, `bounded` with an expiry time, `acknowledged` or `completed`. The overall status is `database_erased` once the transaction commits, and `complete` only when every applicable entry is completed. A deployment with a registered store it cannot account for cannot report `complete`.

**Fence first.** The fence commits in PostgreSQL without depending on Kafka or Temporal. Neither outage can delay a withdrawal or erasure taking effect. A journal outage blocks starting an erasure (section 9).

**Kafka**

- Payloads are opaque references and metadata. Partition keys are opaque too, because keys persist in logs. **Raw personal payloads are prohibited in Kafka.** Source backfill and CDC write through the tenant-bound API or a tenant PostgreSQL staging area and publish references. The staging area is a register entry with retention bounded to at most the longest of Kafka and dead-letter retention, and erasure searches it by identifier fingerprint. Kafka therefore does not buffer raw records during a PostgreSQL outage, and sources must retry. This narrows the target in PR #102. An exception needs an amendment.
- Queue state is never authority. Each consumer rechecks tenant, recovery generation, source epoch, lifecycle and permission against the tenant database before any effect, and dispatch rechecks immediately before the external call.
- A send that began before a withdrawal or erasure committed cannot be recalled. It is recorded, and a suppression or deletion is sent downstream. Reports must not claim otherwise.
- An event for an erased subject is discarded without effect, and a create hits the identifier fence. Historical imports cannot override a later withdrawal (section 5) or restore erased data (section 10).
- Consumer dedupe is retained at least as long as the longest replay window.
- Compaction and tombstones are not erasure, and a retained log generally cannot remove one record. A Kafka copy is `bounded` until retention has elapsed across topics, dead-letter queues, mirrors and backups, unless key destruction is available and all copies are demonstrably controlled.

**Retention defaults**, configurable below these upper bounds and to be validated by M3 measurement:

| Store | Default | Upper bound |
| --- | --- | --- |
| Kafka topics | 7 days | 14 days |
| Kafka dead-letter queues | 14 days | 30 days |
| Kafka backups | None; rebuilt from the outbox | Requires an amendment |
| Temporal closed-workflow history and visibility | 7 days | 30 days |
| Temporal archival | Off | Requires an amendment |
| Temporal workflow lifetime | Bounded; long waits live in the obligation ledger | 30 days |

Compaction is not used as a privacy mechanism.

**Temporal**

- Workflow IDs, arguments, activity results, failure messages, memos and search attributes contain only opaque references and status. Activities fetch data from the tenant database under current authorization.
- The PostgreSQL obligation ledger, rebuilt from the journal after a restore, is the authority and workflows execute it. Cancelling a workflow completes nothing and leaves its obligations open.
- Every activity attempt rereads recovery generation, lifecycle and permission immediately before an external effect and uses an idempotency key. The fence is in the activity, not in workflow code, because replay returns recorded results.
- Partial failure moves forward only. Erasure is never compensated by restoring data.
- Histories, visibility and any archival have registered retention, and workflow lifetimes are bounded.
- While a tenant is suspended, or after a restore until the journal is reapplied, workers perform no external effects.

### 18. Stage gates

M2 does not wait for M3, because an M2 deployment has no Kafka or Temporal entries in its register. That does not make erasure complete at commit. Kratos, mail, logs and backups are register entries in every deployment, so `complete` normally waits for acknowledgment and backup expiry.

| Stage | Must be true before the stage is accepted |
| --- | --- |
| **M2** (PostgreSQL only, both products) | Sections 1–14 implemented. Deployment binding enforced. Data-store register with its mandatory entries, and an obligation ledger. A test that every configured endpoint is registered and that `complete` stays false while any entry is not completed. Outbox records carry lifecycle and source epochs. The recovery check refuses an unreconciled database, and the restore command is its only writer. The restore harness covers the journal, intent reconciliation, the unjournaled window, rehearsal isolation, and withdraw-then-restore. No Kafka, Temporal or Hydra code. |
| **M3 contract** (Kafka and Temporal, both products) | No tenant-derived data enters either until its stores are registered with retention. Typed allowlists for envelopes, keys, workflow inputs, results and search attributes, tested in CI. Fault injection proves: withdrawal and erasure while queued, activity retry after either, replay after erasure, duplicate delivery, broker and Temporal outage, crash between commit and publish, and restore with retained events. Receipt expiry ([#86](https://github.com/buelbuel/customer-fabric/issues/86)) stays closed until its horizon is proven. |
| **M5** (production, both products) | Encryption and key custody for all registered stores. Retention enforcement measured on real broker and history storage, including backups. Restore drills across PostgreSQL, Kafka and Temporal. [#70](https://github.com/buelbuel/customer-fabric/issues/70) and [#104](https://github.com/buelbuel/customer-fabric/issues/104) closed or formally accepted. |
| **Cloud release** (Cloud only) | Sections 1 and 15 proven by [#100](https://github.com/buelbuel/customer-fabric/issues/100) with two or more synthetic tenants: wrong issuer, audience, database credential, topic and namespace all fail closed; one-tenant restore and termination leave others untouched; staff membership revocation takes effect within the configured maximum staleness; cookies stay host-only per tenant. Provider access decision made. |

Self-hosted never requires the control plane. An M2 self-hosted deployment needs neither Kafka nor Temporal. From M3 the full CDC and CDP function needs both, run or sourced by the operator, with a supported single-node profile ([#54](https://github.com/buelbuel/customer-fabric/issues/54), [#64](https://github.com/buelbuel/customer-fabric/issues/64)).

## Recorded decisions and open items

Maintainer decisions:

| Question | Decision |
| --- | --- |
| A legal hold against another subject's records (2026-11-08) | Split into subscopes. Independent records proceed. Shared or ambiguous records stay protected for review. Data whose separation cannot be proven is never erased |
| Journal unavailable (2026-11-08) | Withdrawals accepted as `pending_journal` only if durably recorded and effective for suppression. A PostgreSQL commit is not rollback-safe. Otherwise fail closed |
| Releasing a legal hold (2026-11-08) | Resumes nothing. Fresh scope evaluation and a recorded authorization decision are required |
| v1 erasure scope (2026-11-09) | Automatic erasure only for proven-exclusive, separable, fenced data. Shared, ambiguous, unattested or held data needs an explicit recorded decision. No claim of completed erasure while obligations are open |
| Withdrawal coverage across associations (2026-11-09) | Derived at evaluation from immutable evidence and permanent association history (section 3), not copied rows |

Proposed for approval: independent journal storage (ADR 0010); objections apply to legitimate-interest and public-task purposes, with compelling grounds except direct marketing (legal review); a recorded controller decision per shared-profile review item; no raw personal data in Kafka; Kafka and Temporal retention as in section 17; one Temporal namespace per tenant; staff assertions of at most five minutes with live checks for erasure, export and secret changes; break-glass provider access of at most four hours; no Kafka or Temporal in self-hosted M2.

Open:
- **Legal:** Cloud controller and processor roles, and retention periods.
- **Product:** the OIDC product (#72), pairwise subject identifiers (#74), and the scope of behavioral events (#77, #78).
- **Real-data blockers:** Kratos #70 and #104.
- **Recovery:** whether M2 ships the independent witness (ADR 0010).

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
| 15 | [#91](https://github.com/buelbuel/customer-fabric/issues/91), [#92](https://github.com/buelbuel/customer-fabric/issues/92), [#93](https://github.com/buelbuel/customer-fabric/issues/93), [#94](https://github.com/buelbuel/customer-fabric/issues/94), [#95](https://github.com/buelbuel/customer-fabric/issues/95), [#98](https://github.com/buelbuel/customer-fabric/issues/98), [#100](https://github.com/buelbuel/customer-fabric/issues/100) |
| 16 | [#72](https://github.com/buelbuel/customer-fabric/issues/72), [#73](https://github.com/buelbuel/customer-fabric/issues/73), [#74](https://github.com/buelbuel/customer-fabric/issues/74) |
| 17 | #27, [#36](https://github.com/buelbuel/customer-fabric/issues/36), [#64](https://github.com/buelbuel/customer-fabric/issues/64), [#96](https://github.com/buelbuel/customer-fabric/issues/96), [#97](https://github.com/buelbuel/customer-fabric/issues/97) |
| 18 | [#8](https://github.com/buelbuel/customer-fabric/issues/8) dependency map |

## Consequences

Consent and lifecycle add domain packages and tables inside the modular monolith. M2 adds no infrastructure beyond journal storage. Kafka and Temporal add real privacy cost: an erasure cannot be reported complete until their retention has elapsed. Shells, fenced receipts, proof fences and fingerprints retain pseudonymous metadata longer than a simple delete would. That is deliberate, and each is bounded by section 12's gate. The sticky attribution flag, shared-profile outcomes and protected subscopes create human review work, a real operational cost that avoids erasing another person's record or claiming an erasure that did not happen. A partly executed erasure can leave a subject `erasure_pending` for as long as a hold or review lasts.

Required evidence before the related work is complete: everything listed in ADR 0003, plus the catalog inventory test failing on an unclassified column; tested foreign-key purge order; withdrawal surviving unlink; withdraw-then-link in both directions; a late source grant failing to override a withdrawal; shared-profile outcomes, including a formerly shared profile staying non-exclusive; re-provisioning during pending Kratos deletion; fenced replay of create, update and evidence; redemption racing erasure; and concurrent erasure against association of the same identifier.
