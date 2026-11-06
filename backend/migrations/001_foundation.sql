-- +goose Up
CREATE TABLE sources (name text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'));
CREATE TABLE profiles (
 id uuid PRIMARY KEY, version bigint NOT NULL CHECK (version > 0),
 attributes jsonb NOT NULL CHECK (jsonb_typeof(attributes) = 'object'),
 created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE external_identifiers (
 source text NOT NULL REFERENCES sources(name), namespace text NOT NULL CHECK (namespace ~ '^[a-z][a-z0-9_]{0,63}$'),
 value text NOT NULL CHECK (octet_length(value) BETWEEN 1 AND 256), profile_id uuid NOT NULL REFERENCES profiles(id),
 PRIMARY KEY (source, namespace, value)
);
CREATE INDEX external_identifiers_profile ON external_identifiers(profile_id);
CREATE TABLE mutations (
 id uuid PRIMARY KEY, source text NOT NULL REFERENCES sources(name), key text NOT NULL CHECK (length(key) BETWEEN 16 AND 128),
 request_hash text NOT NULL, profile_id uuid NOT NULL REFERENCES profiles(id),
 status integer NOT NULL CHECK (status IN (200,201,409)), result jsonb NOT NULL,
 created_at timestamptz NOT NULL DEFAULT now(), UNIQUE(source,key)
);
CREATE TABLE attribute_ownership (
 profile_id uuid NOT NULL REFERENCES profiles(id), path text NOT NULL, source text NOT NULL REFERENCES sources(name),
 tombstone boolean NOT NULL DEFAULT false, PRIMARY KEY(profile_id,path)
);
CREATE TABLE provenance (
 sequence bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
 profile_id uuid NOT NULL REFERENCES profiles(id), path text NOT NULL, source text NOT NULL REFERENCES sources(name),
 mutation_id uuid NOT NULL REFERENCES mutations(id), at timestamptz NOT NULL DEFAULT now(),
 outcome text NOT NULL CHECK (outcome IN ('accepted','rejected')), rule text NOT NULL CHECK(rule = 'first-writer-owns')
);
CREATE INDEX provenance_profile ON provenance(profile_id,sequence);
CREATE TABLE outbox (
 id uuid PRIMARY KEY REFERENCES mutations(id), type text NOT NULL CHECK(type IN ('profile.created.v1','profile.updated.v1')),
 profile_id uuid NOT NULL REFERENCES profiles(id), version bigint NOT NULL, source text NOT NULL REFERENCES sources(name),
 occurred_at timestamptz NOT NULL DEFAULT now(), payload jsonb NOT NULL, UNIQUE(profile_id,version)
);

-- +goose Down
-- Destructive: only use on disposable databases. Production recovery is roll-forward.
DROP TABLE outbox;
DROP TABLE provenance;
DROP TABLE attribute_ownership;
DROP TABLE mutations;
DROP TABLE external_identifiers;
DROP TABLE profiles;
DROP TABLE sources;
