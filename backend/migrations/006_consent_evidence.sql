-- +goose Up
-- Consent subjects and append-only evidence (#23). Additive only; ADR 0009 sections 3 and 5.
-- Coverage across associations is derived from evidence and association history at evaluation time
-- (#24), so no fan-out rows are written here.

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
-- Precedence uses received_at, so only the database clock may set it: a caller-supplied or
-- source timestamp can never reorder decisions.
-- +goose StatementBegin
CREATE FUNCTION consent_evidence_received_at() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  NEW.received_at := clock_timestamp();
  RETURN NEW;
END $$;
CREATE TRIGGER consent_evidence_received_at BEFORE INSERT ON consent_evidence
  FOR EACH ROW EXECUTE FUNCTION consent_evidence_received_at();
-- +goose StatementEnd

-- Association history is consent evidence too: evaluation derives which record decisions cover a
-- customer from it (#24), so an association row is never deleted, its subject, record and link time
-- never change, and it can only be revoked once. Link and revocation times come from the database
-- clock, taken while the M1 link paths and record evidence writes hold the same record-epoch lock.
-- +goose StatementBegin
CREATE FUNCTION verified_association_history() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'verified association history is permanent';
  END IF;
  IF TG_OP = 'INSERT' THEN
    NEW.linked_at := clock_timestamp();
    IF NEW.revoked_at IS NOT NULL THEN
      RAISE EXCEPTION 'a verified association starts active';
    END IF;
    RETURN NEW;
  END IF;
  IF (NEW.id, NEW.grant_id, NEW.customer_subject, NEW.source, NEW.record_profile_id, NEW.record_epoch, NEW.linked_at)
     IS DISTINCT FROM (OLD.id, OLD.grant_id, OLD.customer_subject, OLD.source, OLD.record_profile_id, OLD.record_epoch, OLD.linked_at)
     OR OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL THEN
    RAISE EXCEPTION 'a verified association can only be revoked once';
  END IF;
  NEW.revoked_at := clock_timestamp();
  RETURN NEW;
END $$;
CREATE TRIGGER verified_association_history BEFORE INSERT OR UPDATE OR DELETE ON verified_associations
  FOR EACH ROW EXECUTE FUNCTION verified_association_history();
-- +goose StatementEnd

-- +goose Down
-- Drops consent subjects and evidence. Use only in a disposable database.
DROP TRIGGER verified_association_history ON verified_associations;
DROP FUNCTION verified_association_history();
DROP TABLE consent_evidence;
DROP FUNCTION consent_evidence_received_at();
DROP FUNCTION consent_evidence_append_only();
DROP TABLE consent_subjects;
