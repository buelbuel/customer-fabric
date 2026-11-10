-- +goose Up
-- Typed attribute definitions (#83, minimum needed for segmentation). A defined attribute's values
-- are validated on every source write; an undefined attribute stays an untyped scalar.

-- Revisions are append-only. Compatibility between them is a domain rule checked under the schema lock.
CREATE TABLE attribute_definitions (
 path text NOT NULL CHECK (path ~ '^[a-z][a-z0-9_]{0,63}$'),
 revision integer NOT NULL CHECK (revision > 0),
 type text NOT NULL CHECK (type IN ('string','integer','decimal','boolean','date','timestamp','enum')),
 max_length integer CHECK (max_length BETWEEN 1 AND 4096),
 enum_values text[] CHECK (cardinality(enum_values) BETWEEN 1 AND 100),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (path, revision),
 CHECK (type = 'string' OR max_length IS NULL),
 CHECK ((type = 'enum') = (enum_values IS NOT NULL))
);
-- +goose StatementBegin
CREATE FUNCTION attribute_definition_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'attribute definition revisions are immutable';
END $$;
CREATE TRIGGER attribute_definition_immutable BEFORE UPDATE OR DELETE ON attribute_definitions
  FOR EACH ROW EXECUTE FUNCTION attribute_definition_immutable();
-- +goose StatementEnd

-- The revision an accepted value was validated under. NULL for removals, rejections and untyped values.
ALTER TABLE provenance ADD COLUMN attribute_revision integer,
  ADD CONSTRAINT provenance_attribute_revision FOREIGN KEY (path, attribute_revision) REFERENCES attribute_definitions(path, revision);

-- Defining an attribute checks the values already stored under its name.
CREATE INDEX attribute_ownership_path ON attribute_ownership(path) WHERE NOT tombstone;

-- +goose Down
-- Drops attribute types and the revision recorded in provenance. Use only in a disposable database.
DROP INDEX attribute_ownership_path;
ALTER TABLE provenance DROP COLUMN attribute_revision;
DROP TABLE attribute_definitions;
DROP FUNCTION attribute_definition_immutable();
