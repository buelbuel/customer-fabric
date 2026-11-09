-- name: ListCustomerViewRecords :many
-- Active associations whose record lifetime still exists. A revoked link, a deleted identifier or a
-- re-created identifier (new lifetime) has no row here.
SELECT a.id AS association_id, a.source, a.linked_at, p.id AS record_profile_id, p.version, p.attributes, p.updated_at,
  (p.creator_source IS NOT DISTINCT FROM a.source)::boolean AS source_owned,
  (SELECT count(*) FROM external_identifiers i WHERE i.profile_id = p.id)::bigint AS identifier_count,
  (coalesce(s.lifecycle_state <> 'active', false) OR p.lifecycle_state <> 'active' OR p.restricted)::boolean AS record_restricted
FROM verified_associations a
JOIN external_identifiers e ON (a.record_epoch, a.source, a.record_profile_id) = (e.record_epoch, e.source, e.profile_id)
JOIN profiles p ON p.id = e.profile_id
LEFT JOIN consent_subjects s ON s.kind = 'source_record' AND s.anchor_record_epoch = a.record_epoch
WHERE a.customer_subject = sqlc.arg(customer_subject)::uuid AND a.revoked_at IS NULL
ORDER BY a.id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListCustomerViewEvidence :many
-- Each live attribute's owner and that owner's newest accepted write. An attribute with no such write
-- has no row, so it cannot be attributed and stays withheld.
SELECT DISTINCT ON (o.profile_id, o.path) o.profile_id, o.path, o.source AS owner, pr.mutation_id, pr.at
FROM attribute_ownership o
JOIN provenance pr ON pr.profile_id = o.profile_id AND pr.path = o.path AND pr.source = o.source AND pr.outcome = 'accepted'
WHERE o.profile_id = ANY(sqlc.arg(profile_ids)::uuid[]) AND NOT o.tombstone
ORDER BY o.profile_id, o.path, pr.sequence DESC;
