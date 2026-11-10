-- name: ListMatchableRecords :many
-- Active, unrestricted source-owned records in a stable order. Customer anchors have no creator source and
-- are never compared.
SELECT id, creator_source::text AS source, attributes FROM profiles
WHERE creator_source IS NOT NULL AND lifecycle_state = 'active' AND NOT restricted
ORDER BY id LIMIT sqlc.arg(row_limit)::int;
