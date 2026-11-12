# ADR 0018 Unified customer profiles: identity graph and attribute resolution

Accepted 2026-11-12 for the synthetic development environment. It supersedes the first-writer ownership rule of [ADR 0001](0001-architecture.md) for values across sources.

## Context

Customer Fabric unifies customer data from independent source systems: CRM, commerce, loyalty and more. Several records describe one customer, and their values overlap and disagree. Records arrive before anyone has configured how to recognise them. Associations can be wrong and must be correctable without losing source data.

## Decision

### Source profiles are durable records

- **What a source profile is.** Every profile created by a source is that source's record: its namespaced identifiers and its own attribute values, with provenance and per-attribute tombstones. Only the source that created it writes it, so values from different sources never overwrite each other.
- **Writes are atomic.** A write is applied whole or rejected whole. It is rejected only for validation (422), an identifier or idempotency conflict (409), a stale version (412) or the lifecycle fence.
- **Writing the unified ID.** A source that writes a unified customer's ID writes its own record in that customer:
  - one source record there: the write goes to it;
  - none: 403, the same answer as for an unknown profile;
  - several: 409 (write the record's own ID instead).
- If-Match always refers to the record written.

A customer's profile anchor ([ADR 0005](0005-customer-profile-linking.md)) is not a source profile. It is never linked and never written by a source.

### The identity graph

**Links.** An identity link connects two source profiles that a trusted identity key says are the same customer.

- A governance admin declares a trusted identity key: an identifier namespace, such as a master customer number, and the 2 to 20 sources trusted to present it.
- Two active, unrestricted source profiles are linked when sources trusted for the key hold the same value in that namespace.
- Each link is stored with its key namespace and its history.

**Unified customers.** The connected profiles form one unified customer.

- Its **canonical ID** is the oldest member's ID; ties go to the lower ID.
- Every other member ID resolves to it as an **alias**. Reads accept any member ID and answer with the canonical ID, so no earlier ID dangles.
- Unification is logical. No profile, identifier, value, provenance, consent record or audit entry is moved, merged or deleted.

**Conflicts are never linked.**

- A link that would give one customer two different values of one trusted key is refused and recorded as a `conflict`. That covers ambiguous and contradictory identities.
- Inactive or restricted profiles are never linked. A profile that becomes so loses its links.
- The deployment's database is one tenant, so a link never crosses tenants.

**Correction.**

- An identity reviewer removes a wrong link, citing the review or ticket it rests on. The customers are recalculated at once, and the pair is not linked by that key again; another trusted key can still link it.
- Removing or changing a key removes the links it no longer supports.

### Reconciliation

Resolution is continuous and covers existing data. A profile is queued for reconciliation in the same transaction when any of these happens:

- a write adds identifiers, a new record's included;
- a key is declared, changed or removed: every profile with identifiers in that namespace is queued;
- its restriction changes.

**The reconciler.** The reconciler in the API process recomputes each queued profile's links from current state. It is idempotent: repeating a step changes nothing. It then recalculates every affected customer.

- **Serialization.** Steps take one graph lock, so they are serialized with each other and with corrections. Profile writes never wait for them.
- **Audit.** Every decision is recorded append-only: link, unlink, conflict, canonical change.
- **Downstream event.** A profile whose canonical ID changes emits `profile.unified.v1` through the outbox, naming its canonical ID and carrying the profile's current version; the relay publishes it after that version's change. Webhook subscriptions carry record changes only.

Configuring keys after the first import is therefore normal: the reconciler resolves what is already there.

### Attribute resolution

A unified customer's attributes are resolved per attribute from its members' values. The members' own values are kept and shown with their sources.

- **Policies.** Each attribute may have a resolution policy:
  - `source_priority`: the first listed source with a value wins; unlisted sources rank after all listed ones;
  - `most_recent`: the most recently updated value wins.
- **Without a policy,** and between equally ranked values, the earliest value still held wins, then the lower record ID. That is the value whose source set it first and has not withdrawn it since. A routine overlap therefore keeps the existing unified value and records the alternative, with no manual step.
- **Determinism.** Resolution depends only on the members' current values, their timestamps and the policies. It is recalculated on every read and always gives the same answer for the same state. Unification and correction never rewrite source values.

## Consequences

- **What changes for sources:**
  - several sources contribute to one customer without approvals, and overlapping fields never block a record or discard others;
  - first-writer ownership across sources is gone;
  - a removal withdraws only that source's value.
- **Readers:**
  - The operator profile API lists unified customers by canonical ID and shows each one: aliases, members with their own values and versions, resolved attributes with the record each comes from, identifiers and provenance. Its identity history lists links and decisions.
  - The governed customer view, matching, consent, segments and activation keep reading source records, unchanged.
- **Not done here:**
  - automatic linking by probabilistic match classes (match reviews stay advisory);
  - resolution policies by verification status;
  - unified views for segmentation.
