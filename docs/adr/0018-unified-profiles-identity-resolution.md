# ADR 0018 Unified profiles and identity resolution

Accepted 2026-11-12 for the synthetic development environment.

## Context

Customer Fabric unifies customer data from independent source systems, so several sources must contribute to one profile. The profile API already keeps field-level ownership:

- the first source that writes an attribute owns it;
- a removal leaves a tombstone;
- every decision, accepted or rejected, is recorded in provenance;
- writes use optimistic concurrency.

Until now there were two models, and neither was the product model:

- in the M0 development mode, any source could write any profile, so a profile ID alone was enough;
- the customer-identity mode refused every source but the profile's creator.

## Decision

### Unified profiles and source associations

- **No owning source.** A profile has no owning source, and creating one confers no exclusive ownership.
- **Source identifiers.** Each source keeps its own namespaced identifiers.
- **Association.** A source is associated with a profile when it holds an identifier on it, or created it. It may then write that profile: update its attributes, add its own identifiers, and send events and import rows for its records there.
- **Unassociated sources.** A source that is not associated gets 403. That is the same answer as for an unknown profile, so knowing a profile ID grants nothing. Adding an identifier through an update never associates a source with a profile.

### Identity resolution

A source becomes associated with an existing profile only through central identity resolution. Resolution runs when the source creates a record, whether through `POST /v1/profiles` or an import row.

- **Trusted identity keys.** A governance admin declares an identifier namespace as a trusted identity key across named sources, for example a master customer number shared by CRM, commerce and loyalty systems.
- **Joining.** If the new record presents such a key, resolution looks up the active, unrestricted source profiles that another trusted source identifies by the same key value. A source's own identifiers are never evidence, and a record that presents an identifier the source already holds is refused as an identifier conflict, so a create can never write an existing profile without its version.
  - **exactly one profile:** the record joins that profile (`associated`);
  - **none:** the record gets a new profile (`created`). This includes a match that is only restricted or not active: the record then gets its own profile, so an unassociated source never learns that profile's state;
  - **keys naming several profiles:** the record also gets a new profile, recorded as `ambiguous`. Nothing is ever merged.
- **No assertion by a source.** A namespace that is not a trusted key never resolves. A source not listed for a key can neither resolve by it nor be matched through its own identifiers in that namespace. A source therefore cannot establish another source's identity by asserting it.
- **Concurrency.** Each key value is resolved under its own lock, taken in a fixed order before any profile lock. Records created at the same time with the same key resolve one after another: one creates the profile and the others join it. A key identifier added later through an update takes no resolution lock and resolves nothing. A record created concurrently with such an update can end up on its own profile, and later records with that key are then recorded as ambiguous, never merged.
- **Audit.** Every decision is recorded append-only with the mutation, source, profile, outcome and key namespace, and is listed per profile for operators. Key changes apply to records created afterwards, never move existing records, and are in the operator audit.

Probabilistic matching (match candidates and reviews) stays advisory. It never associates anything.

### Enforcement

Association is decided inside the mutation's transaction, checked once before and again after the profile's row lock (so an unassociated source never waits on or contends for a profile), for every write path: profile update, source event and import row. Field ownership, tombstones, provenance and optimistic concurrency then decide each write exactly as before, so a joined source cannot overwrite, remove or resurrect another source's attributes. A customer's profile anchor has no creating source and carries no source identifiers: no source writes it or resolves to it ([ADR 0005](0005-customer-profile-linking.md)).

### Source events

A source's event resolves its record by the source-scoped identifier on whichever source profile it is on. Event order is per record, so a source may have only one event-ordered record per profile. An event for a second record of the same source is refused once another record of that source on the profile already has an order. Aliases and trusted-key identifiers without events do not conflict, and ownership keeps different sources' records apart.

## Consequences

- One profile carries identifiers and attributes from several sources through the public API. Every record created with a trusted key has a recorded resolution decision.
- Routine integrations need no operator approval per profile, only the one-time declaration of trusted keys.
- **Not handled here:**
  - merging profiles after an ambiguous or late match;
  - moving a record between profiles;
  - attribute-based keys, such as verified email.

  Each would be a further identity-resolution operation with its own evidence and audit.
