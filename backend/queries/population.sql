-- Set-based reads for evaluating customer populations in chunks. Each mirrors a single-customer query
-- (named below) and keeps its per-customer bound, so a chunk decides exactly what one-by-one reads would.

-- name: ListCustomerAnchorsAfter :many
-- Customer anchors in a stable order, by keyset.
SELECT profile_id, kratos_identity_id FROM customer_identity_links
WHERE profile_id > sqlc.arg(after)::uuid ORDER BY profile_id LIMIT sqlc.arg(row_limit)::int;

-- name: CountCustomerAnchors :one
SELECT count(*)::bigint FROM customer_identity_links;

-- name: ListCustomerSubjectsByAnchors :many
-- GetCustomerSubjectByAnchor for many anchors.
SELECT anchor_profile_id::uuid AS anchor_profile_id, id, lifecycle_state FROM consent_subjects
WHERE kind = 'customer' AND anchor_profile_id = ANY(sqlc.arg(anchor_ids)::uuid[]);

-- name: ListCustomerViewRecordsFor :many
-- ListCustomerViewRecords for many customers, at most row_limit records each.
SELECT r.customer_subject, r.association_id, r.source, r.linked_at, r.record_profile_id, r.version, r.attributes, r.updated_at,
  r.source_owned, r.identifier_count, r.record_restricted FROM (
  SELECT a.customer_subject, a.id AS association_id, a.source, a.linked_at, p.id AS record_profile_id, p.version, p.attributes, p.updated_at,
    (p.creator_source IS NOT DISTINCT FROM a.source)::boolean AS source_owned,
    (SELECT count(*) FROM external_identifiers i WHERE i.profile_id = p.id)::bigint AS identifier_count,
    (coalesce(s.lifecycle_state <> 'active', false) OR p.lifecycle_state <> 'active' OR p.restricted)::boolean AS record_restricted,
    row_number() OVER (PARTITION BY a.customer_subject ORDER BY a.id) AS n
  FROM verified_associations a
  JOIN external_identifiers e ON (a.record_epoch, a.source, a.record_profile_id) = (e.record_epoch, e.source, e.profile_id)
  JOIN profiles p ON p.id = e.profile_id
  LEFT JOIN consent_subjects s ON s.kind = 'source_record' AND s.anchor_record_epoch = a.record_epoch
  WHERE a.customer_subject = ANY(sqlc.arg(customer_subjects)::uuid[]) AND a.revoked_at IS NULL) r
WHERE r.n <= sqlc.arg(row_limit)::int
ORDER BY r.customer_subject, r.association_id;

-- name: ListCustomerEvidenceFor :many
-- ListCustomerEvidence for many subjects, at most row_limit rows each.
SELECT r.subject_id, r.decision, r.received_at, r.actor_kind, r.quarantined, r.version, r.lawful_basis FROM (
  SELECT e.subject_id, e.decision, e.received_at, e.actor_kind, e.quarantined, pv.version, pv.lawful_basis,
    row_number() OVER (PARTITION BY e.subject_id) AS n
  FROM consent_evidence e
  JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
  WHERE e.subject_id = ANY(sqlc.arg(subject_ids)::uuid[]) AND pv.purpose_id = sqlc.arg(purpose_id)::uuid) r
WHERE r.n <= sqlc.arg(row_limit)::int;

-- name: ListCarriedRecordEvidenceFor :many
-- ListCarriedRecordEvidence for many customers, at most row_limit rows each.
SELECT r.customer_subject, r.decision, r.effective_at, r.actor_kind, r.quarantined, r.version, r.lawful_basis FROM (
  SELECT a.customer_subject, e.decision, greatest(e.received_at, a.linked_at)::timestamptz AS effective_at, e.actor_kind, e.quarantined,
    pv.version, pv.lawful_basis, row_number() OVER (PARTITION BY a.customer_subject) AS n
  FROM verified_associations a
  JOIN consent_subjects s ON s.kind = 'source_record' AND s.anchor_record_epoch = a.record_epoch
  JOIN consent_evidence e ON e.subject_id = s.id
  JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
  WHERE a.customer_subject = ANY(sqlc.arg(customer_subjects)::uuid[]) AND pv.purpose_id = sqlc.arg(purpose_id)::uuid
    AND e.decision IN ('withdraw','deny','object')
    AND (a.revoked_at IS NULL OR e.received_at < a.revoked_at)) r
WHERE r.n <= sqlc.arg(row_limit)::int;

-- name: InsertSnapshotMembers :exec
INSERT INTO segment_snapshot_members (snapshot_id, anchor_profile_id)
SELECT sqlc.arg(snapshot_id)::uuid, unnest(sqlc.arg(anchor_ids)::uuid[]);

-- name: InsertDeliveries :exec
INSERT INTO activation_deliveries (id, activation_id, anchor_profile_id)
SELECT unnest(sqlc.arg(ids)::uuid[]), sqlc.arg(activation_id)::uuid, unnest(sqlc.arg(anchor_ids)::uuid[]);
