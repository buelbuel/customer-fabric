-- +goose Up
-- Audience snapshots (#43, slice 2 of #123): a segment's membership frozen at one evaluation, as anchor
-- IDs only, never attribute values. Comparing two snapshots shows who joined and who left. Snapshots
-- are immutable; their retention is governed with the rest of M2 lifecycle work (#28).
CREATE TABLE segment_snapshots (
 id uuid PRIMARY KEY,
 segment_name text NOT NULL,
 segment_revision integer NOT NULL,
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 taken_at timestamptz NOT NULL DEFAULT now(),
 population integer NOT NULL CHECK (population >= 0),
 members integer NOT NULL CHECK (members >= 0),
 excluded_unavailable integer NOT NULL CHECK (excluded_unavailable >= 0),
 excluded_consent integer NOT NULL CHECK (excluded_consent >= 0),
 excluded_criteria integer NOT NULL CHECK (excluded_criteria >= 0),
 FOREIGN KEY (segment_name, segment_revision) REFERENCES segment_definitions(name, revision)
);
CREATE INDEX segment_snapshots_by_segment ON segment_snapshots(segment_name, taken_at DESC);

CREATE TABLE segment_snapshot_members (
 snapshot_id uuid NOT NULL REFERENCES segment_snapshots(id),
 anchor_profile_id uuid NOT NULL REFERENCES profiles(id),
 PRIMARY KEY (snapshot_id, anchor_profile_id)
);
-- +goose StatementBegin
CREATE FUNCTION segment_snapshot_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'segment snapshots are immutable';
END $$;
CREATE TRIGGER segment_snapshot_immutable BEFORE UPDATE OR DELETE ON segment_snapshots
  FOR EACH ROW EXECUTE FUNCTION segment_snapshot_immutable();
CREATE TRIGGER segment_snapshot_member_immutable BEFORE UPDATE OR DELETE ON segment_snapshot_members
  FOR EACH ROW EXECUTE FUNCTION segment_snapshot_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops snapshots. Use only in a disposable database.
DROP TABLE segment_snapshot_members;
DROP TABLE segment_snapshots;
DROP FUNCTION segment_snapshot_immutable();
