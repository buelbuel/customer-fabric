-- +goose Up
-- Segment definitions (#42, Epic 3 of #117). A segment is a typed rule over governed attributes plus
-- the consent purpose its members must permit. Revisions are append-only; the latest is current.
-- Membership is not stored: a preview evaluates the current revision on every read.
CREATE TABLE segment_definitions (
 name text NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
 revision integer NOT NULL CHECK (revision > 0),
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 rule jsonb NOT NULL CHECK (jsonb_typeof(rule) = 'object'),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (name, revision)
);
-- +goose StatementBegin
CREATE FUNCTION segment_definition_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'segment definition revisions are immutable';
END $$;
CREATE TRIGGER segment_definition_immutable BEFORE UPDATE OR DELETE ON segment_definitions
  FOR EACH ROW EXECUTE FUNCTION segment_definition_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops segment definitions. Use only in a disposable database.
DROP TABLE segment_definitions;
DROP FUNCTION segment_definition_immutable();
