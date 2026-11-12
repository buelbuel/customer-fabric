-- +goose Up
-- Identity resolution for unified profiles (ADR 0018). A profile has no owning source: each source keeps
-- its own namespaced identifiers, and a source may write any profile it is associated with, by creating it
-- or by holding an identifier on it. A source becomes associated with an existing profile only through
-- resolution: when it creates a record that presents a trusted identity key, the record joins the one
-- profile that trusted sources already identify by that key. No match creates a profile; several matches
-- create one too and are recorded as ambiguous, never merged.

-- Trusted identity keys: identifier namespaces that name the same person across the listed sources, such
-- as a master customer number. Only the listed sources' identifiers count, and only they may resolve by it.
CREATE TABLE identity_resolution_keys (
 namespace text PRIMARY KEY CHECK (namespace ~ '^[a-z][a-z0-9_]{0,63}$'),
 sources text[] NOT NULL CHECK (cardinality(sources) BETWEEN 2 AND 20),
 changed_by text NOT NULL CHECK (char_length(changed_by) BETWEEN 1 AND 128),
 changed_at timestamptz NOT NULL DEFAULT now()
);

-- One append-only decision per record that presented a trusted key: which profile it joined or created.
CREATE TABLE identity_resolutions (
 id uuid PRIMARY KEY,
 mutation_id uuid NOT NULL REFERENCES mutations(id),
 source text NOT NULL REFERENCES sources(name),
 profile_id uuid NOT NULL REFERENCES profiles(id),
 outcome text NOT NULL CHECK (outcome IN ('created','associated','ambiguous')),
 key_namespace text NOT NULL,
 decided_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX identity_resolutions_profile ON identity_resolutions(profile_id, decided_at);
CREATE INDEX external_identifiers_key ON external_identifiers(namespace, value);
-- +goose StatementBegin
CREATE FUNCTION identity_resolution_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'identity resolutions are append-only';
END $$;
CREATE TRIGGER identity_resolution_immutable BEFORE UPDATE OR DELETE ON identity_resolutions
  FOR EACH ROW EXECUTE FUNCTION identity_resolution_immutable();
-- +goose StatementEnd

-- +goose Down
DROP INDEX external_identifiers_key;
DROP TABLE identity_resolutions;
DROP FUNCTION identity_resolution_immutable();
DROP TABLE identity_resolution_keys;
