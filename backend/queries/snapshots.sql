-- name: InsertSnapshot :one
INSERT INTO segment_snapshots (id, segment_name, segment_revision, purpose_id, population, members, excluded_unavailable, excluded_consent, excluded_criteria)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING taken_at;


-- name: GetSnapshot :one
SELECT * FROM segment_snapshots WHERE id = $1;

-- name: ListSnapshots :many
SELECT * FROM segment_snapshots WHERE segment_name = $1 ORDER BY taken_at DESC, id LIMIT 100;

-- name: ListSnapshotDifference :many
-- One page of the members of one snapshot that are not members of the other, by anchor keyset.
SELECT m.anchor_profile_id FROM segment_snapshot_members m
WHERE m.snapshot_id = sqlc.arg(included)::uuid AND m.anchor_profile_id > sqlc.arg(after)::uuid
  AND NOT EXISTS (SELECT 1 FROM segment_snapshot_members o WHERE o.snapshot_id = sqlc.arg(excluded)::uuid AND o.anchor_profile_id = m.anchor_profile_id)
ORDER BY m.anchor_profile_id LIMIT sqlc.arg(row_limit)::int;
