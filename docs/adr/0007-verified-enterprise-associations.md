# ADR 0007 Verified enterprise associations

Accepted for the M1 synthetic development environment. Extends [ADR 0005](0005-customer-profile-linking.md) with verified external relationships. The customer subject and profile anchor remain stable.

## Association boundary

A Kratos identity establishes a session and verification of its current mailbox. A Customer Fabric profile stores business attributes. A source record is the source's own account or relationship. In this slice the issuer selects one canonical external identifier per source record. The tuple of source, namespace and value resolves that identifier, while an internally assigned UUID represents the lifetime of that specific record binding. Issuers must canonicalize aliases before issuing proof. Different sources may refer to the same person without sharing record identity.

A verified association relates a customer subject to one live source-record binding. Potential matches derived from email, phone, names, historical references or record IDs convey no authority. Candidate search, attribute reconciliation and profile merge require separate policies. No matching engine is required for this slice.

Customer-facing association reads expose the association ID, source name and time. They confer no access to enterprise attributes, source identifiers or provenance. The separate, previously provisioned customer profile stays the same. The source retains its creator and first-writer authorizations. This operation has no integration outbox event. Grant state and association history commit together in PostgreSQL.

## Authority and grant

The deployment configures an issuer credential per source in `CF_DEV_LINKING_ISSUERS`. Issuer credentials differ from every mutation and operator credential. Only the corresponding issuer can create or revoke grants for that source. The credential belongs to the source's proof-verifying service. A customer uses their verified Kratos identity and independently proves control of a specific source record to that service before the service attests the proof. An opaque UUID identifies the completed proof, is unique per issuer, and contains no evidence or claimant data. The API trusts the authenticated issuer's decision and cannot independently certify the issuer's upstream verification. Possession of a customer email, source ID, or ordinary source write token cannot issue a grant.

The test authority demonstrates this boundary using an out-of-band, single-use synthetic source challenge and a live Kratos session. It derives the subject from Kratos after verifying the challenge. It has no access to fixture ground truth. There is no Salesforce, ERP, Shopify or other real proof adapter. The supported proof method is `synthetic_challenge`; the API and credential configuration require explicit development mode.

The server returns a 256-bit random grant once. PostgreSQL stores its SHA-256 digest, opaque grant ID, issuer, record lifetime, target profile ID, proof reference and method, subject and timestamps. The audience and purpose are fixed to customer association redemption on this deployment. A grant expires ten minutes after issuance. Deliver it privately through the trusted service, in memory and in the request body. Do not put it in URLs, logs, profile records or provenance. Lost delivery requires new proof. Credential rotation preserves the configured issuer source. Removing a source's issuer configuration disables redemption for that source while absent. Revoke affected outstanding grants before restoring a disabled issuer or retiring compromised credentials. The development API has no production credential lifecycle.

## Transactions and failure behavior

Issue, redeem, cancel and unlink serialize by source-record lifetime. PostgreSQL locks the external identifier during issue and redemption. A partial unique index permits one active association per record lifetime. The atomic redemption consumes exactly one usable grant, inserts the link and revokes other pending grants for that record. If any write fails, the transaction rolls back. A new grant requires fresh proof. A duplicate proof reference cannot issue a second grant. Redeeming the same secret concurrently yields at most one success. A lost redemption response can be reconciled through the subject-scoped list rather than replaying the secret.

Invalid, expired, consumed, revoked, wrong-subject, or unavailable-record grants yield the same conflict without disclosing other customers. The customer must provision their own profile before redemption. Revoked and missing associations are unavailable through customer reads. Issuers can revoke pending grants or resulting links. A customer can unlink only their own association. Repeated revocations succeed and keep the initial revocation time and actor. Revocation does not transfer a record or retract prior activity. Another subject needs freshly verified evidence to link again.

A deleted source identifier suppresses existing association reads and blocks redemption. A re-created identifier receives a fresh lifetime UUID even if it points to the same profile. Its predecessor's proof and active association do not revive. Changing record ownership outside this add-only API would require a reviewed lifecycle operation that rotates the record lifetime and revokes stale links. Loss of external source access must be reported by the trusted authority through revocation. The application cannot infer upstream state.

## Lifecycle and handoff

Verification and recovery follow the active Kratos subject, so an email or password change does not transfer an association. During loss of current-email verification, the customer API fails closed. Deactivation or session revocation blocks access online. Re-registration under a different subject has no inherited link. Identity deletion is distinct from unlinking or profile erasure.

Grant and association metadata contain pseudonymous identifiers. M1 retains audit timestamps and references for synthetic testing. Before real data, [ADR 0003](0003-data-lifecycle.md) requires purpose and retention rules for proof receipts and revoked/expired grants, association history, Kratos records, profile links, provenance, outbox and mutation receipts. M2 erasure must fence active claims and source replay, coordinate across these records and honor backup expiry. Restoring a backup must reapply revocation and erasure fences before enabling access. No consent or erasure endpoint is introduced here.
