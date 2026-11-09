-- +goose Up
-- Consent subjects and append-only evidence (#23). Additive only; ADR 0009 sections 3 and 5.

-- A consent subject has exactly one anchor: a customer anchor profile, or a source record epoch. ADR 0009 section 3.
-- Lifecycle states exist as a column but nothing in this change transitions to erasure_pending or erased.
CREATE TABLE consent_subjects (
 id uuid PRIMARY KEY,
 kind text NOT NULL CHECK (kind IN ('customer','source_record')),
 lifecycle_state text NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active','erasure_pending','erased')),
 anchor_profile_id uuid REFERENCES profiles(id),
 anchor_record_epoch uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 CHECK (
   (kind = 'customer' AND anchor_profile_id IS NOT NULL AND anchor_record_epoch IS NULL) OR
   (kind = 'source_record' AND anchor_profile_id IS NULL AND anchor_record_epoch IS NOT NULL)
 )
);
CREATE UNIQUE INDEX consent_subjects_customer_anchor ON consent_subjects(anchor_profile_id)
  WHERE kind = 'customer';
CREATE UNIQUE INDEX consent_subjects_record_anchor ON consent_subjects(anchor_record_epoch)
  WHERE kind = 'source_record';

-- Append-only evidence. Never updated or deleted; no raw credentials or contact values. ADR 0009 section 5.
CREATE TABLE consent_evidence (
 id uuid PRIMARY KEY,
 subject_id uuid NOT NULL REFERENCES consent_subjects(id),
 policy_version_id uuid NOT NULL REFERENCES consent_policy_versions(id),
 decision text NOT NULL CHECK (decision IN ('grant','deny','withdraw','object')),
 actor_kind text NOT NULL CHECK (actor_kind IN ('customer','source','operator')),
 source text REFERENCES sources(name),
 source_epoch uuid,
 occurred_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 proof_channel text NOT NULL CHECK (proof_channel ~ '^[a-z][a-z0-9_]{0,31}$'),
 proof_reference text NOT NULL CHECK (octet_length(proof_reference) BETWEEN 1 AND 256),
 idempotency_key text NOT NULL CHECK (octet_length(idempotency_key) BETWEEN 8 AND 128),
 quarantined boolean NOT NULL DEFAULT false,
 UNIQUE (subject_id, idempotency_key)
);
CREATE INDEX consent_evidence_subject ON consent_evidence(subject_id, policy_version_id, received_at DESC);
-- +goose StatementBegin
CREATE FUNCTION consent_evidence_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'consent evidence is append-only';
END $$;
CREATE TRIGGER consent_evidence_append_only BEFORE UPDATE OR DELETE ON consent_evidence
  FOR EACH ROW EXECUTE FUNCTION consent_evidence_append_only();
-- +goose StatementEnd

-- Link-time and receive-time materialization across an active association (ADR 0009 section 3).
CREATE TABLE consent_evidence_materializations (
 id uuid PRIMARY KEY,
 evidence_id uuid NOT NULL REFERENCES consent_evidence(id),
 subject_id uuid NOT NULL REFERENCES consent_subjects(id),
 association_id uuid NOT NULL REFERENCES verified_associations(id),
 materialized_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 UNIQUE (evidence_id, subject_id)
);
-- +goose StatementBegin
CREATE FUNCTION consent_materialization_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'consent materializations are append-only';
END $$;
CREATE TRIGGER consent_materialization_append_only BEFORE UPDATE OR DELETE ON consent_evidence_materializations
  FOR EACH ROW EXECUTE FUNCTION consent_materialization_append_only();
-- +goose StatementEnd

-- +goose Down
-- Drops consent subjects and evidence. Use only in a disposable database.
DROP TABLE consent_evidence_materializations;
DROP FUNCTION consent_materialization_append_only();
DROP TABLE consent_evidence;
DROP FUNCTION consent_evidence_append_only();
DROP TABLE consent_subjects;
