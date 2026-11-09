-- +goose Up
-- Consent purpose registry (#22). Additive only: no erasure, hold, restriction, restore or journal
-- state. ADR 0009 section 4. The deployment and controller binding of section 1, with its startup
-- check, is separate work and is not created here.

-- A purpose is a stable identifier scoped to this data plane's controller. ADR 0009 section 4.
CREATE TABLE consent_purposes (
 id uuid PRIMARY KEY,
 name text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
 created_at timestamptz NOT NULL DEFAULT now()
);

-- Policy versions are immutable once active. Lawful basis is a property of the version.
CREATE TABLE consent_policy_versions (
 id uuid PRIMARY KEY,
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 version integer NOT NULL CHECK (version > 0),
 state text NOT NULL CHECK (state IN ('draft','active','retired')),
 lawful_basis text NOT NULL CHECK (lawful_basis IN
   ('consent','contract','legal_obligation','vital_interests','public_task','legitimate_interests')),
 notice_reference text NOT NULL CHECK (octet_length(notice_reference) BETWEEN 1 AND 256),
 jurisdiction text NOT NULL CHECK (jurisdiction ~ '^[A-Z]{2}$'),
 locale text NOT NULL CHECK (locale ~ '^[a-z]{2}(-[A-Z]{2})?$'),
 channel text NOT NULL CHECK (channel ~ '^[a-z][a-z0-9_]{0,31}$'),
 retention_reference text NOT NULL CHECK (octet_length(retention_reference) BETWEEN 1 AND 256),
 carry_forward boolean NOT NULL DEFAULT false,
 created_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (purpose_id, version)
);
-- At most one active version per purpose; activating a new one requires retiring the old first.
CREATE UNIQUE INDEX consent_policy_one_active ON consent_policy_versions(purpose_id)
  WHERE state = 'active';
-- An active or retired version is immutable.
-- +goose StatementBegin
CREATE FUNCTION consent_policy_version_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  -- The only permitted change to an active version is the state transition to retired.
  IF OLD.state = 'active' THEN
    IF NEW.state <> 'retired' OR (NEW.state, NEW.id, NEW.purpose_id, NEW.version, NEW.lawful_basis,
        NEW.notice_reference, NEW.jurisdiction, NEW.locale, NEW.channel, NEW.retention_reference,
        NEW.carry_forward) IS DISTINCT FROM
        ('retired', OLD.id, OLD.purpose_id, OLD.version, OLD.lawful_basis, OLD.notice_reference,
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
CREATE TRIGGER consent_policy_version_immutable BEFORE UPDATE ON consent_policy_versions
  FOR EACH ROW EXECUTE FUNCTION consent_policy_version_immutable();
-- +goose StatementEnd
-- A version that was ever active is part of the notice record and is never deleted.
-- +goose StatementBegin
CREATE FUNCTION consent_policy_version_kept() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.state <> 'draft' THEN
    RAISE EXCEPTION 'active or retired consent policy versions cannot be deleted';
  END IF;
  RETURN OLD;
END $$;
CREATE TRIGGER consent_policy_version_kept BEFORE DELETE ON consent_policy_versions
  FOR EACH ROW EXECUTE FUNCTION consent_policy_version_kept();
-- +goose StatementEnd

-- +goose Down
-- Drops the consent registry. Use only in a disposable database.
DROP TABLE consent_policy_versions;
DROP FUNCTION consent_policy_version_kept();
DROP FUNCTION consent_policy_version_immutable();
DROP TABLE consent_purposes;
