# ADR 0018 Cross-source profile contributions

Accepted 2026-11-12 for the synthetic development environment.

## Context

Customer Fabric unifies customer data from independent source systems, so several sources must be able to contribute to one profile. The profile API already keeps attribute-level ownership: the first source that writes an attribute owns it, a removal leaves a tombstone, and every decision, accepted or rejected, is recorded in provenance. In the M0 development mode, any source could write any profile. A profile ID alone was then enough to write, and the separate customer-identity mode refused every source but the creator. Neither is the product model.

Verified associations ([ADR 0007](0007-verified-enterprise-associations.md)) relate a customer to a source record and authorize no writes. Match reviews record an operator's decision that two records are, or are not, the same person, and authorize nothing on their own.

## Decision

A profile has no exclusive owning source. A source may write a profile in two cases:

- it created the profile;
- it holds an active **contributor authorization** for that profile.

A contributor adds its own identifiers, in its own source scope, and its own attributes. Attribute ownership, tombstones and provenance decide each write exactly as they do for the creator. A contributor therefore cannot overwrite, remove or resurrect an attribute another source owns. Such a write is refused with 409 and recorded as rejected.

An **identity reviewer** (`identity_reviewer`) authorizes a contributor through `POST /v1/operator/profiles/{id}/contributors`. The authorization rests on a verified identity association, recorded as its basis:

- **`reviewed_match`** cites a match review between this profile and a profile the source created. The review must be the pair's latest decision, and it must be `same_person`.
- **`verified_reference`** cites a verification made outside this API, for example the source's own identity verification, for a source that has no record of its own yet.

Rules:

- **No ID-based access.** Knowing a profile ID grants nothing. A write to a profile the source may not write gets 403, the same answer as for an unknown profile.
- **No authorization for the creator.** The creator needs none.
- **No contributors on a customer's profile anchor.** An anchor has no creating source, and no source ever writes it ([ADR 0005](0005-customer-profile-linking.md)).
- **One active authorization per profile and source.**

**Revocation.** `POST …/contributors/{contributor}/revoke`, citing a reference, ends an authorization:

- Every later write by that source to the profile is refused, whether by update, source event or import.
- Nothing already written is retracted. The source keeps owning the attributes it wrote, and its identifiers stay attached, unable to write.
- A later `different_people` review of a pair revokes every authorization resting on an earlier review of that pair, in the same transaction, with reason `match_review_reversed`.
- An authorization changes only by its single revocation. The operator audit records who authorized and who revoked.

**Enforcement.** The decision is taken inside the mutation's transaction, after taking the profile's row lock. That covers every write path: profile update, source event and import row. Authorizing and revoking take the same lock, so a write waiting on the lock sees a revocation that committed while it waited.

**Source events.** A source's event resolves the record by the source-scoped identifier, whichever profile it is attached to. A record attached to a customer anchor is never resolved. The ordering rule now counts only the event's own source: two identifiers of one source on a profile have no single order. Other sources' records on the same profile write other attributes, because ownership keeps sources apart.

## Consequences

- One profile can hold identifiers and attributes from several sources through the public API, and every contribution is traceable to an authorization.
- Identity reviewers carry the decision that two sources describe the same person. Proof adapters that would verify such an association automatically (for example a customer proving control of both records) can later create the same authorization with a new basis.
- Governed views already show a value only for the source that owns it, so they need no change. Consent stays per record lifetime, so a contributed identifier's lifetime must permit a purpose on its own evidence.
- Merging two existing profiles, moving ownership between sources and splitting a profile remain out of scope.
