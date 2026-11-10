-- +goose Up
-- Resumable source imports (#33, slice 1 of #123). A source pushes NDJSON rows in ordered batches; each
-- row commits with the import's checkpoint, so a restarted client resumes exactly after the last
-- committed row. Raw rows are never stored: quarantine keeps a line number and a reason code only.
CREATE TABLE source_imports (
 id uuid PRIMARY KEY,
 source text NOT NULL REFERENCES sources(name),
 state text NOT NULL DEFAULT 'open' CHECK (state IN ('open','completed','failed')),
 checkpoint bigint NOT NULL DEFAULT 0 CHECK (checkpoint >= 0),
 applied bigint NOT NULL DEFAULT 0,
 created bigint NOT NULL DEFAULT 0,
 quarantined bigint NOT NULL DEFAULT 0,
 max_errors integer NOT NULL CHECK (max_errors BETWEEN 0 AND 100000),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE source_import_quarantine (
 import_id uuid NOT NULL REFERENCES source_imports(id),
 line bigint NOT NULL CHECK (line > 0),
 reason text NOT NULL CHECK (reason ~ '^[a-z_]{1,64}$'),
 PRIMARY KEY (import_id, line)
);

-- +goose Down
-- Drops import progress. Use only in a disposable database.
DROP TABLE source_import_quarantine;
DROP TABLE source_imports;
