-- name: LockSegment :exec
-- Serializes revisions of one segment.
SELECT pg_advisory_xact_lock(hashtextextended('segment:' || sqlc.arg(name)::text, 0));

-- name: InsertSegmentRevision :one
INSERT INTO segment_definitions (name, revision, purpose_id, rule) VALUES ($1, $2, $3, $4) RETURNING created_at;

-- name: ListSegmentRevisions :many
SELECT * FROM segment_definitions WHERE name = $1 ORDER BY revision;

-- name: GetCurrentSegment :one
SELECT * FROM segment_definitions WHERE name = $1 ORDER BY revision DESC LIMIT 1;

-- name: ListCurrentSegments :many
SELECT DISTINCT ON (name) * FROM segment_definitions ORDER BY name, revision DESC;

-- name: ListCustomerAnchors :many
-- Every customer anchor a segment could include, in a stable order.
SELECT profile_id, kratos_identity_id FROM customer_identity_links ORDER BY profile_id LIMIT sqlc.arg(row_limit)::int;
