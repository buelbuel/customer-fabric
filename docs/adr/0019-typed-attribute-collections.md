# ADR 0019: Bounded typed attribute collections

Accepted for the synthetic development environment. It supersedes the "arrays are unsupported" limitation of [ADR 0001](0001-architecture.md) for attributes defined with `max_items`; ADR 0018's per-source contribution and resolution rules are unchanged.

## Decision

An attribute definition may declare `max_items` from 1 to 100. Its value is then an ordered JSON array, including an empty array, of non-null scalars of the declared type. The existing per-value encoded limit of 4096 bytes applies to the whole array; string length and enum constraints apply to each element. Nested arrays and objects are not supported. An undefined attribute remains a scalar. Null is not a removal; use `remove`.

The existing immutable definition revisions and schema lock apply to all ingestion paths. A collection bound may increase, but type and scalar/collection cardinality cannot change; incompatible changes require a new name and explicit source-side migration. Source contributions, resolution and provenance apply to the entire ordered array, not individual elements. Existing access policies govern the entire value; no per-element policy is implied. Collection conditions in segmentation support only presence and absence; CSV mapping does not encode arrays.

## Consequences

This enables bounded multi-valued fields without introducing unbounded JSON or changing source ownership, identity, consent or lifecycle decisions. Objects, addresses, normalization and mapping, per-element provenance, PII classification, purpose-level field authorization, and incompatible in-place migration remain future work in #83. The governed verified-association view and the logical unified profile retain their distinct existing authorization and resolution semantics. Production use remains blocked by #70 and #104 and the other documented security and privacy gates.
