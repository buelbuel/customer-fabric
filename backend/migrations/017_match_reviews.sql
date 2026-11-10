-- +goose Up
-- Operator review of match candidates (#41): an append-only trail of decisions. A decision records what
-- the operator concluded and the evidence they saw; it never links, merges or authorizes anything.
CREATE TABLE match_reviews (
 id uuid PRIMARY KEY,
 profile_id uuid NOT NULL REFERENCES profiles(id),
 candidate_profile_id uuid NOT NULL REFERENCES profiles(id),
 decision text NOT NULL CHECK (decision IN ('same_person','different_people')),
 class text NOT NULL CHECK (class IN ('strong','probable','conflicting')),
 agree text[] NOT NULL,
 disagree text[] NOT NULL,
 reviewed_at timestamptz NOT NULL DEFAULT now(),
 CHECK (profile_id <> candidate_profile_id)
);
CREATE INDEX match_reviews_profile ON match_reviews(profile_id, reviewed_at DESC);
CREATE INDEX match_reviews_candidate ON match_reviews(candidate_profile_id, reviewed_at DESC);
-- +goose StatementBegin
CREATE FUNCTION match_review_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'match reviews are append-only';
END $$;
CREATE TRIGGER match_review_immutable BEFORE UPDATE OR DELETE ON match_reviews
  FOR EACH ROW EXECUTE FUNCTION match_review_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops the review trail. Use only in a disposable database.
DROP TABLE match_reviews;
DROP FUNCTION match_review_immutable();
