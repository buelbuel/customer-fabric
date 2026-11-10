-- +goose Up
-- Audit of administrative operator writes (#80, slice 3 of #134): who did what to which resource, never
-- the request body. Append-only.
CREATE TABLE operator_audit (
 id uuid PRIMARY KEY,
 actor text NOT NULL CHECK (actor = 'development-bearer' OR actor ~ '^[0-9a-f-]{36}$'),
 method text NOT NULL CHECK (method IN ('POST','PUT','PATCH','DELETE')),
 resource text NOT NULL CHECK (octet_length(resource) <= 512),
 status integer NOT NULL CHECK (status BETWEEN 200 AND 299),
 at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX operator_audit_recent ON operator_audit(at DESC);
-- +goose StatementBegin
CREATE FUNCTION operator_audit_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'operator audit is append-only';
END $$;
CREATE TRIGGER operator_audit_immutable BEFORE UPDATE OR DELETE ON operator_audit
  FOR EACH ROW EXECUTE FUNCTION operator_audit_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops the operator audit. Use only in a disposable database.
DROP TABLE operator_audit;
DROP FUNCTION operator_audit_immutable();
