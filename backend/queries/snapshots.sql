-- name: InsertSnapshot :one
INSERT INTO segment_snapshots (id, segment_name, segment_revision, purpose_id, population, members, excluded_unavailable, excluded_consent, excluded_criteria)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING taken_at;

-- name: InsertSnapshotMember :exec
INSERT INTO segment_snapshot_members (snapshot_id, anchor_profile_id) VALUES ($1, $2);

-- name: GetSnapshot :one
SELECT * FROM segment_snapshots WHERE id = $1;

-- name: ListSnapshots :many
SELECT * FROM segment_snapshots WHERE segment_name = $1 ORDER BY taken_at DESC, id LIMIT 100;

-- name: ListSnapshotDifference :many
-- Members of one snapshot that are not members of the other.
SELECT m.anchor_profile_id FROM segment_snapshot_members m WHERE m.snapshot_id = sqlc.arg(included)::uuid
EXCEPT
SELECT o.anchor_profile_id FROM segment_snapshot_members o WHERE o.snapshot_id = sqlc.arg(excluded)::uuid
ORDER BY 1 LIMIT sqlc.arg(row_limit)::int;
