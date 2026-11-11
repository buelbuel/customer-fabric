-- name: GetProfileLifecycleState :one
SELECT lifecycle_state, restricted, version FROM profiles WHERE id = $1;

-- name: InsertNotificationSubscription :one
INSERT INTO notification_subscriptions (id, name, destination, purpose_id, policy_version_id, basis_assessment, recipient_role, event_types,
  data_categories, sources, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11) ON CONFLICT (name) DO NOTHING RETURNING *;

-- name: GetNotificationSubscription :one
SELECT * FROM notification_subscriptions WHERE id = $1;

-- name: GetNotificationSubscriptionByName :one
SELECT * FROM notification_subscriptions WHERE name = $1;

-- name: ListNotificationSubscriptions :many
SELECT * FROM notification_subscriptions ORDER BY name;

-- name: ListSubscriptionsForEvent :many
-- Active subscriptions that may receive an event of this type from this source. The notifier decides
-- again at every attempt; this only selects whom to try.
SELECT id FROM notification_subscriptions
WHERE state = 'active' AND sqlc.arg(event_type)::text = ANY(event_types) AND sqlc.arg(source)::text = ANY(sources)
ORDER BY id;

-- name: SetNotificationSubscriptionState :one
UPDATE notification_subscriptions SET state = $2, updated_at = now() WHERE name = $1 RETURNING *;

-- name: GetNotificationDelivery :one
SELECT * FROM notification_deliveries WHERE event_id = $1 AND subscription_id = $2;

-- name: RecordNotificationDelivery :execrows
-- Written once per event and subscription; a later attempt to record the same pair changes nothing.
INSERT INTO notification_deliveries (event_id, subscription_id, profile_id, profile_version, outcome, reason, attempts, status_code)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) ON CONFLICT (event_id, subscription_id) DO NOTHING;

-- name: ListNotificationDeliveries :many
SELECT * FROM notification_deliveries WHERE subscription_id = $1 ORDER BY recorded_at DESC, event_id LIMIT 100;

-- name: ListRecordConsentScope :many
-- For one source record: each of its record epochs, the record subject anchored on it (if any) and the
-- customer it is verifiably associated with now (if any), with that customer's anchor profile.
SELECT e.record_epoch, s.id AS subject_id, s.lifecycle_state AS subject_state,
  a.customer_subject, l.profile_id AS anchor_profile_id
FROM external_identifiers e
LEFT JOIN consent_subjects s ON s.kind = 'source_record' AND s.anchor_record_epoch = e.record_epoch
LEFT JOIN verified_associations a ON a.record_epoch = e.record_epoch AND a.revoked_at IS NULL
LEFT JOIN customer_identity_links l ON l.kratos_identity_id = a.customer_subject
WHERE e.profile_id = $1
ORDER BY e.record_epoch;

-- name: ListRecordSubjectEvidence :many
-- Consent evidence on the given record subjects for one purpose, at most row_limit rows.
-- verified_source marks a source grant made under a consent authority that is active now and was granted
-- to the same source for the same purpose.
SELECT e.subject_id, e.decision, e.received_at, e.actor_kind, e.quarantined, pv.version, pv.lawful_basis,
  (e.actor_kind = 'source' AND coalesce(a.state = 'active' AND a.source = e.source AND a.purpose_id = pv.purpose_id, false))::boolean AS verified_source
FROM consent_evidence e
JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
LEFT JOIN consent_authorities a ON a.id = e.authority_id
WHERE e.subject_id = ANY(sqlc.arg(subject_ids)::uuid[]) AND pv.purpose_id = sqlc.arg(purpose_id)::uuid
LIMIT sqlc.arg(row_limit)::int;

-- name: ListRegisteredSources :many
SELECT name FROM sources WHERE name = ANY(sqlc.arg(names)::text[]);
