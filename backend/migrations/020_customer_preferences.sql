-- +goose Up
-- Customer-owned profile fields (#75, slice 4 of #134). The customer alone sets them, separately from
-- Kratos identity traits and from source-owned attributes. The change log names the fields each version
-- changed, never their values.
CREATE TABLE customer_preferences (
 anchor_profile_id uuid PRIMARY KEY REFERENCES profiles(id),
 display_name text CHECK (char_length(display_name) BETWEEN 1 AND 100),
 preferred_language text CHECK (preferred_language ~ '^[A-Za-z]{2,3}(-[A-Za-z0-9]{1,8})*$' AND char_length(preferred_language) <= 35),
 timezone text CHECK (char_length(timezone) BETWEEN 1 AND 64),
 version bigint NOT NULL CHECK (version > 0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE customer_preference_changes (
 anchor_profile_id uuid NOT NULL REFERENCES profiles(id),
 version bigint NOT NULL CHECK (version > 0),
 fields text[] NOT NULL CHECK (cardinality(fields) BETWEEN 1 AND 3 AND fields <@ ARRAY['display_name','preferred_language','timezone']::text[]),
 changed_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (anchor_profile_id, version)
);
-- +goose StatementBegin
CREATE FUNCTION customer_preference_change_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'customer preference changes are append-only';
END $$;
CREATE TRIGGER customer_preference_change_immutable BEFORE UPDATE OR DELETE ON customer_preference_changes
  FOR EACH ROW EXECUTE FUNCTION customer_preference_change_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops customer-owned fields. Use only in a disposable database.
DROP TABLE customer_preference_changes;
DROP TABLE customer_preferences;
DROP FUNCTION customer_preference_change_immutable();
