# ADR 0017: Trust in source-originated consent

Status: accepted for implementation (#154, extending ADR 0015). The evidence-verification half is not implemented yet. Until it is, and until the legal and security release gates are met, no real customer data may be processed on source-originated consent.

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
- **Audit.** Every change to an authority or its keys is audited, with the operator and reference: approved, renewed, suspended, resumed, revoked, key_added, key_retired, key_compromised. Approvals and renewals also record the arrangement and review date then in force.
- **Record lifetimes.** A receipt binds the record's identifier, not its lifetime. Identifiers are never deleted today, so a grant cannot reach a later lifetime. Any future identifier deletion or erasure must refuse grants collected before the new lifetime began.
- **Existing data.** Authorities approved before vetting was recorded are suspended by the migration. Grants recorded before receipts were signed no longer count.

### Part 2: evidence verification, trust policy, rate limits (next; production blockers)

- **Verifiable evidence contract.** A structured proof document (`customer-fabric.consent-proof.v1`) that the source produces on request.
  - **Checks:** Customer Fabric checks the document's digest against the recorded one, its subject binding to the same record lifetime, the notice and policy version, the method, the collection time and the controller.
  - **Custody:** the document is not stored; only the result, method, reviewer, time and references are kept.
  - **Fail-closed outcomes:** a failed check, or a request unanswered by its deadline, stops the grant from counting.
- **Trust policy per authority.**
  - **`attestation`:** a source attestation suffices operationally, subject to spot checks.
  - **`verified`:** a grant counts only once its proof has been verified.
- **Risk-based audits.** Sampled verification requests against an authority. Failures suspend the authority pending review.
- **Rate limits and abuse monitoring.** Per-source limits on consent decisions, and alerts on refused receipts and failed verifications.
- **Obligations.** Evidence custody and verification duties in the controller and processor agreements, retention, erasure, backup and the M2/M5 release gates.

## Consequences

- A source grant is attributable to a registered key and to a vetted, reviewed and revocable authority. Its trust level is explicit.
- Compromise of a source key, a source incident or a lapsed review stops its grants from authorizing anything, at once.
- Part 2 remains a blocker for any real-data use of source-originated consent (#154).
