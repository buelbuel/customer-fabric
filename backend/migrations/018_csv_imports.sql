-- +goose Up
-- CSV source imports (#33): an import declares its format, and a CSV import its explicit column mapping.
ALTER TABLE source_imports ADD COLUMN format text NOT NULL DEFAULT 'ndjson' CHECK (format IN ('ndjson','csv')),
  ADD COLUMN mapping jsonb CHECK ((format = 'csv') = (mapping IS NOT NULL));

-- +goose Down
-- Drops import formats. Use only in a disposable database.
ALTER TABLE source_imports DROP COLUMN mapping, DROP COLUMN format;
