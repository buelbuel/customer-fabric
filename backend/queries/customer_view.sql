
-- name: ListCustomerViewEvidence :many
-- Each live attribute's owner and that owner's newest accepted write. An attribute with no such write
-- has no row, so it cannot be attributed and stays withheld. Unless all_paths is set, only the named
-- paths are read.
SELECT DISTINCT ON (o.profile_id, o.path) o.profile_id, o.path, o.source AS owner, pr.mutation_id, pr.at, pr.attribute_revision
FROM attribute_ownership o
JOIN provenance pr ON pr.profile_id = o.profile_id AND pr.path = o.path AND pr.source = o.source AND pr.outcome = 'accepted'
WHERE o.profile_id = ANY(sqlc.arg(profile_ids)::uuid[]) AND NOT o.tombstone
  AND (sqlc.arg(all_paths)::boolean OR o.path = ANY(sqlc.arg(paths)::text[]))
ORDER BY o.profile_id, o.path, pr.sequence DESC;
