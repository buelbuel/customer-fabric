-- +goose Up
-- Cross-source profile contributions (ADR 0018). A profile has no exclusive owning source: besides the
-- source that created it, any source an identity reviewer has authorized may add its own identifiers and
-- attributes, under the same attribute ownership, provenance and tombstone rules. An authorization rests
-- on a verified identity association, never on knowledge of a profile ID:
--   - reviewed_match: a same_person match review between this profile and a profile the source created;
--   - verified_reference: a verification the reviewer records by reference (for a source with no record
--     of its own yet).
-- Revocation stops future writes and never retracts what was written. Authorizations are append-only
-- except for their single revocation.
CREATE TABLE profile_contributors (
 id uuid PRIMARY KEY,
 profile_id uuid NOT NULL REFERENCES profiles(id),
 source text NOT NULL REFERENCES sources(name),
 basis text NOT NULL CHECK (basis IN ('reviewed_match','verified_reference')),
 match_review_id uuid REFERENCES match_reviews(id),
 verification_reference text CHECK (verification_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 authorized_by text NOT NULL CHECK (char_length(authorized_by) BETWEEN 1 AND 128),
 authorized_at timestamptz NOT NULL DEFAULT now(),
 revoked_by text CHECK (char_length(revoked_by) BETWEEN 1 AND 128),
 revoked_at timestamptz,
 revocation_reason text CHECK (revocation_reason ~ '^[a-z_]{1,64}$'),
 revocation_reference text CHECK (revocation_reference ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 CHECK ((basis = 'reviewed_match') = (match_review_id IS NOT NULL)),
 CHECK (basis <> 'verified_reference' OR verification_reference IS NOT NULL),
 CHECK ((revoked_at IS NULL) = (revoked_by IS NULL) AND (revoked_at IS NULL) = (revocation_reason IS NULL))
);
CREATE UNIQUE INDEX profile_contributors_active ON profile_contributors(profile_id, source) WHERE revoked_at IS NULL;
CREATE INDEX profile_contributors_review ON profile_contributors(match_review_id) WHERE revoked_at IS NULL;

-- +goose StatementBegin
CREATE FUNCTION profile_contributor_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' OR OLD.revoked_at IS NOT NULL OR NEW.revoked_at IS NULL OR
     (NEW.id, NEW.profile_id, NEW.source, NEW.basis, NEW.match_review_id, NEW.verification_reference, NEW.authorized_by, NEW.authorized_at)
     IS DISTINCT FROM
     (OLD.id, OLD.profile_id, OLD.source, OLD.basis, OLD.match_review_id, OLD.verification_reference, OLD.authorized_by, OLD.authorized_at) THEN
    RAISE EXCEPTION 'profile contributor authorizations change only by one revocation';
  END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER profile_contributor_append_only BEFORE UPDATE OR DELETE ON profile_contributors
  FOR EACH ROW EXECUTE FUNCTION profile_contributor_append_only();
-- +goose StatementEnd

-- +goose Down
DROP TABLE profile_contributors;
DROP FUNCTION profile_contributor_append_only();
