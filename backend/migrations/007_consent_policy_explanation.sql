-- +goose Up
-- The customer-readable explanation shown with a purpose (#25). It belongs to the policy version
-- because a decision is evidence about what the customer was shown. The schema is unreleased and no
-- text can stand in for a notice, so this fails on a database that already holds policy versions.
ALTER TABLE consent_policy_versions ADD COLUMN explanation text NOT NULL
  CONSTRAINT consent_policy_versions_explanation CHECK (octet_length(explanation) BETWEEN 1 AND 2048);
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION consent_policy_version_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- The only permitted change to an active version is the state transition to retired.
  IF OLD.state = 'active' THEN
    IF NEW.state <> 'retired' OR (NEW.state, NEW.id, NEW.purpose_id, NEW.version, NEW.lawful_basis,
        NEW.notice_reference, NEW.explanation, NEW.jurisdiction, NEW.locale, NEW.channel, NEW.retention_reference,
        NEW.carry_forward) IS DISTINCT FROM
        ('retired', OLD.id, OLD.purpose_id, OLD.version, OLD.lawful_basis, OLD.notice_reference, OLD.explanation,
         OLD.jurisdiction, OLD.locale, OLD.channel, OLD.retention_reference, OLD.carry_forward) THEN
      RAISE EXCEPTION 'an active consent policy version may only move to retired';
    END IF;
  END IF;
  -- A retired version is fully immutable.
  IF OLD.state = 'retired' AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'a retired consent policy version is immutable';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Removes customer explanations. Use only in a disposable database.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION consent_policy_version_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.state = 'active' THEN
    IF NEW.state <> 'retired' OR (NEW.state, NEW.id, NEW.purpose_id, NEW.version, NEW.lawful_basis,
        NEW.notice_reference, NEW.jurisdiction, NEW.locale, NEW.channel, NEW.retention_reference,
        NEW.carry_forward) IS DISTINCT FROM
        ('retired', OLD.id, OLD.purpose_id, OLD.version, OLD.lawful_basis, OLD.notice_reference,
         OLD.jurisdiction, OLD.locale, OLD.channel, OLD.retention_reference, OLD.carry_forward) THEN
      RAISE EXCEPTION 'an active consent policy version may only move to retired';
    END IF;
  END IF;
  IF OLD.state = 'retired' AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'a retired consent policy version is immutable';
  END IF;
  RETURN NEW;
END $$;
-- +goose StatementEnd
ALTER TABLE consent_policy_versions DROP COLUMN explanation;
