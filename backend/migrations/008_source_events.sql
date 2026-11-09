-- +goose Up
-- Source event ingestion (#34), with the fences it needs from #84 and #35. A receipt holds no
-- attribute values, so a stale event is decided and audited without retaining its payload.

-- Operator-advanced and independent of any credential, so it survives rotation.
ALTER TABLE sources ADD COLUMN epoch bigint NOT NULL DEFAULT 1 CHECK (epoch > 0);

-- Nothing transitions these yet; every profile write checks them after taking the profile lock.
ALTER TABLE profiles
  ADD COLUMN lifecycle_state text NOT NULL DEFAULT 'active' CHECK (lifecycle_state IN ('active','erasure_pending','erased')),
  ADD COLUMN restricted boolean NOT NULL DEFAULT false;

-- One per record lifetime: a recreated identifier has a new epoch and inherits no ordering.
-- Separate from receipts so ordering outlives any future receipt expiry.
CREATE TABLE source_record_watermarks (
 record_epoch uuid PRIMARY KEY,
 source_epoch bigint NOT NULL CHECK (source_epoch > 0),
 sequence bigint NOT NULL CHECK (sequence > 0),
 updated_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE source_events (
 source text NOT NULL REFERENCES sources(name),
 event_id text NOT NULL CHECK (event_id ~ '^[A-Za-z0-9._:-]{1,128}$'),
 digest text NOT NULL CHECK (length(digest) = 64),
 record_epoch uuid NOT NULL,
 profile_id uuid NOT NULL REFERENCES profiles(id),
 source_epoch bigint NOT NULL CHECK (source_epoch > 0),
 sequence bigint NOT NULL CHECK (sequence > 0),
 occurred_at timestamptz NOT NULL,
 received_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 outcome text NOT NULL CHECK (outcome IN ('applied','stale','conflict')),
 mutation_id uuid REFERENCES mutations(id),
 profile_version bigint,
 PRIMARY KEY (source, event_id),
 CHECK ((outcome = 'stale') = (mutation_id IS NULL)),
 CHECK ((outcome = 'applied') = (profile_version IS NOT NULL))
);
CREATE INDEX source_events_record ON source_events(record_epoch, source_epoch, sequence);

-- +goose Down
-- Drops receipts and ordering fences. Use only in a disposable database.
DROP TABLE source_events;
DROP TABLE source_record_watermarks;
ALTER TABLE profiles DROP COLUMN restricted, DROP COLUMN lifecycle_state;
ALTER TABLE sources DROP COLUMN epoch;
