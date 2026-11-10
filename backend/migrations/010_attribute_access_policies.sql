-- +goose Up
-- Who may read a defined attribute, and whose values (#76, Epic 2 of #117). Without a policy nothing
-- is readable. Policies are append-only revisions; the latest is current and may narrow access.
CREATE TABLE attribute_access_policies (
 path text NOT NULL CHECK (path ~ '^[a-z][a-z0-9_]{0,63}$'),
 revision integer NOT NULL CHECK (revision > 0),
 readers text[] NOT NULL CHECK (readers <@ ARRAY['operator','segmentation','activation']::text[]),
 sources text[] NOT NULL CHECK (cardinality(sources) <= 20),
 created_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (path, revision)
);
-- +goose StatementBegin
CREATE FUNCTION attribute_access_policy_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'attribute access policy revisions are immutable';
END $$;
CREATE TRIGGER attribute_access_policy_immutable BEFORE UPDATE OR DELETE ON attribute_access_policies
  FOR EACH ROW EXECUTE FUNCTION attribute_access_policy_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops attribute access policies. Use only in a disposable database.
DROP TABLE attribute_access_policies;
DROP FUNCTION attribute_access_policy_immutable();
