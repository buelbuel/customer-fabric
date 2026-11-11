# ADR 0015: Governed consent evidence from source systems

Status: accepted for implementation (maintainer direction of 2026-11-11, #64). It completes the source-consent part that ADR 0011 left open ("source consent ingestion … needs its own scoped credential design"). Real customer data stays disallowed until the release gates are met.

## Context

Until now only an authenticated customer's own grant counted as consent. A source record not linked to a verified customer therefore never had consent, whatever the enterprise system that holds it had collected. Customer Fabric must accept verified consent collected by enterprise source systems, including for unlinked records. It must never turn a CRM opt-in flag or an operator's entry into consent.

## Decision

- **Consent authority.** A source may report consent grants for a purpose only under an active consent authority.
  - **Approval:** a `privacy_admin` approves it at `/v1/operator/consent-authorities`. It names the source, the consent-based purpose, the **controller** the consent is collected for, and the approved **collection methods**: `double_opt_in`, `signed_form`, `recorded_web_form` or `recorded_call`. A boolean field in a source system is not a method.
  - **Uniqueness:** one authority per source and purpose is active at a time.
  - **Revocation:** revoking an authority stops every grant made under it from counting, at once and without rewriting evidence.
- **Source decisions.** A source reports decisions about its own records at `POST /v1/source-consent-decisions`, authenticated by its own credential, with an `Idempotency-Key`.
  - **Subject binding:** a decision names the record by the source's own identifier and binds to that identifier's current record lifetime. It takes the same record lock as linking, so its receipt time orders against association history. A record recreated with a new lifetime inherits nothing, and another source's records are not found.
  - **Replay protection:**
    - a decision must carry the source's current epoch, so decisions queued before a source re-keyed are refused;
    - a repeated key returns the original receipt, and a key reused with different content, including a different notice or proof digest, is refused;
    - precedence uses Customer Fabric's receipt time, never the source's clock.
  - **A grant is recorded only when all of these hold:**
    - the source holds an active authority for the purpose;
    - the method is one the authority approves;
    - the notice reference is exactly the one the policy version records;
    - it is against the active version;
    - its collection time lies between the version's creation and now;
    - it carries the source's own receipt reference and a SHA-256 digest of the stored proof artefact.

    A database constraint enforces the authority and proof fields as well. The proof itself stays with the source, under the controller's retention; Customer Fabric keeps the reference and digest that identify it.
  - **Withdrawals, denials and objections are always recorded**, with or without an authority, because stopping processing never needs one.
- **One evaluation engine.** A grant counts as consent only if it is an authenticated customer's grant, or a source's grant made under an authority that is active now (`VerifiedSource`). In both cases it must also be on the subject itself, not carried from a linked record, and not quarantined. Operator grants and unverified source grants never count.
  - Everything else is unchanged: carry-forward, withdrawal precedence by receipt time, objections and basis changes.
  - The record subject is evaluated as itself, so a verified source grant authorizes processing of that record lifetime.
- **Linked records follow the customer.** Once a record lifetime is verifiably associated with a customer, the customer's own effective permission decides it. A source's grant is not carried to the customer; protective decisions are, as before.

## Consequences

- External change notifications (ADR 0014) under a consent-based purpose can now reach unlinked records that carry verified source consent. They stop when the source withdraws, the authority is revoked, or the record is linked to a customer who has not consented.
- The data inventory lists consent authorities and the proof references and digests kept with source decisions.
- **Not covered here:**
  - verifying the proof artefact itself;
  - signed consent receipts from the source;
  - a per-source rate limit;
  - controller-to-controller transfer agreements.

  These are release-gate questions for real data.
