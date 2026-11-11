# ADR 0017: Trust in source-originated consent

Status: accepted for implementation (#154, extending ADR 0015). Both parts are implemented for synthetic data. No real customer data may be processed on source-originated consent until the contractual, retention and legal obligations below and the security and legal release gates are met.

## Context

ADR 0015 lets an approved source report consent it collected. The source sends:

- a receipt reference;
- the notice it showed;
- the collection method and time;
- a SHA-256 digest of a proof artefact that only the source holds.

Customer Fabric checks the syntax and the source's authority. It does not see the artefact.

- **What a digest is:** an integrity reference. It pins which artefact the source claims to hold.
- **What it is not:** proof that the artefact exists or shows valid consent.

Enterprise customers must not need consumer accounts, or to upload every consent document, for consent-based processing. The trust placed in a source's word must therefore be explicit, bounded, revocable and auditable, and stronger checks must be possible where that trust is not enough.

## Decision

### Trust levels

Every source grant is one of:

- **source-attested**: authenticated and bound to its decision, but taken on the source's word;
- **artefact-verified**: Customer Fabric has independently checked its proof, which is part 2 below.

Operator grants and opt-in fields in source data are never consent at any level.

- **API:** the decision receipt says `"trust": "source_attested"`.
- **Docs and operations:** they never describe a digest as verified proof.

### Part 1: authority vetting, signed receipts, lifecycle (implemented)

- **Vetting before authority.** Approving a consent authority records:
  - the controller;
  - the approved purpose and methods;
  - a reference to the arrangement under which the source collects consent for the controller;
  - a reference to the record of the source's vetting;
  - a review date at most a year ahead.

  Past that date the authority authorizes nothing, neither new grants nor existing ones, until a privacy admin renews it with a new review.

  Approving a source verifies none of its grants.
- **Suspend, renew, revoke.**
  - **Suspend:** a privacy admin can suspend an authority, citing the incident or review. Its grants stop counting at once, and it accepts none.
  - **Renew:** renewal resumes it.
  - **Revoke:** revocation is final, and a new authority does not revive old grants.
- **Signed receipts.** Each authority holds one or more Ed25519 public keys, each with a source-chosen `key_id`. A grant must carry a signature, by an active key, over a canonical receipt:
  - **Format:** a fixed prefix followed by the unpadded base64url of each field, joined with dots, which is identical in any language.
  - **Fields bound:** the source, the record's namespace and value, the policy version, the decision, `collected_at` as sent, the epoch, the method, the source's reference, the notice, the digest and the idempotency key.

  An unsigned, forged, wrongly keyed or moved receipt is refused (422 or 403) and logged with its reason, without values.

  A registered key must be a canonically encoded point of full order: a small-order key, such as the identity, would let anyone produce signatures that verify. A public key is registered once, ever, so a retired or compromised key cannot return under another `key_id` or authority.

  A grant is appended under a shared lock on its authority, so it is ordered against suspension, revocation and key changes. The signature is stored with the evidence. The receipt proves where the attestation came from and that it is intact; it does not prove the attested event happened.
- **Key rotation and compromise.** Several keys can be active, so a source rotates without a gap. Retirement and compromise, like suspension and revocation, cite the review, incident or request behind them.
  - **Retired key:** it signs no new grants, but its grants keep counting.
  - **Compromised key:** every grant it signed stops counting at once. Compromise is final.
- **Revocation on every read.** One evaluation engine applies all of this, through the existing `VerifiedSource` input. A grant counts only while its authority:
  - is active;
  - is within its review date;
  - matches the grant's source and purpose;
  - and its signing key is not compromised.

  Every authorization, including each notification attempt (ADR 0014, 0016), reads these live.
- **Audit.** Every change to an authority or its keys is audited, with the operator and reference: approved, renewed, suspended, resumed, revoked, key_added, key_retired, key_compromised. Approvals and renewals also record the arrangement, review date and trust policy then in force.
- **Record lifetimes.** A receipt binds the record's identifier, not its lifetime. Identifiers are never deleted today, so a grant cannot reach a later lifetime. Any future identifier deletion or erasure must refuse grants collected before the new lifetime began.
- **Existing data.** Authorities approved before vetting was recorded are suspended by the migration. Grants recorded before receipts were signed no longer count.

### Part 2: evidence verification, trust policy, rate limits (implemented)

- **Trust policy per authority**, set on approval and on every renewal:
  - **`attestation`:** a signed grant counts until a verification of it fails or goes unanswered.
  - **`verified`:** a grant counts only once a privacy admin reviewed its proof and recorded it verified. The source is asked for the proof as the grant is recorded, in the same transaction, and the receipt (and any retry of it) names that verification.
- **Verifiable evidence contract.** On request a source answers with a proof document, `customer-fabric.consent-proof.v1`, holding:
  - the record's identifier;
  - the policy version;
  - the notice;
  - the method;
  - the collection time;
  - the controller;
  - its receipt reference;
  - the method's own evidence, a non-empty object such as a double opt-in's confirmation timestamps and message reference.
- **Checks on the document.** Customer Fabric checks that:
  - the SHA-256 of the exact bytes sent equals the digest the grant recorded;
  - the record is the identifier of the grant's own record lifetime;
  - every other field matches the grant and its authority.

  The document is then discarded; only the result, its digest and the time are kept.
- **Checked is not verified.** Every field the automatic check compares was attested by the source itself, so a consistent document is `checked`: it shows the source can produce the evidence it committed to, not that consent occurred. Only a privacy admin's review makes a verification `verified` or `failed`. The admin obtains the proof from the source under the controller arrangement, identifies it by the recorded digest, and cites the review's reference. Customer Fabric therefore holds no proof files, yet any grant's proof can be checked and reviewed when needed.
- **Requests.** Verification is requested in three ways, each due in seven days:
  - by a privacy admin for one grant (dispute, audit or access request);
  - by a risk-based audit that samples up to 100 grants never checked before;
  - by the `verified` policy.

  One verification per grant is open at a time. The source lists its open requests by its own reference and record.
- **Fail closed.** These void the grant for good through the same `VerifiedSource` input:
  - a failed check;
  - a request still unanswered at its deadline;
  - an answer after the deadline;
  - a failed review.

  A failure also suspends the authority, audited with the verification or review and its actor, until a privacy admin reviews and renews it. A verification unanswered past its deadline stops every grant of its authority, and the authority accepts none, until a privacy admin closes it as failed. Ignoring a request is therefore never better than answering it.
- **Rate limits and monitoring.** Each source may make 120 consent grants, verification listings and proofs per minute; beyond that, 429 with `Retry-After`. Withdrawals, denials and objections are never limited. Counters are kept per source and minute and pruned after an hour. The limit being reached, refused receipts and failed verifications are logged with the source and a reason, never values.
- **Not decided here, still blocking real data:**
  - evidence custody and verification duties in the controller and processor agreements;
  - the retention of verification records and rate counters (#28);
  - erasure (#27);
  - backup and restoration (#31);
  - legal review of the proof contract per method and jurisdiction;
  - an external alert channel;
  - the M2/M5 release gates.

## Consequences

- A source grant is attributable to a registered key and to a vetted, reviewed and revocable authority. Its trust level is explicit.
- Compromise of a source key, a source incident or a lapsed review stops its grants from authorizing anything, at once.
- A grant's trust is explicit and checkable on demand without central proof storage. The obligations listed above remain blockers for any real-data use of source-originated consent (#154).
