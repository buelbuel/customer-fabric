-- name: GetProfileLifecycleState :one
SELECT lifecycle_state, restricted, version FROM profiles WHERE id = $1;

-- name: InsertNotificationSubscription :one
INSERT INTO notification_subscriptions (id, name, destination, purpose_id, policy_version_id, basis_assessment, recipient_role, event_types,
  data_categories, sources, created_by, replaces)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) ON CONFLICT (name) DO NOTHING RETURNING *;

-- name: GetNotificationSubscription :one
SELECT * FROM notification_subscriptions WHERE id = $1;

-- name: GetNotificationSubscriptionByName :one
SELECT * FROM notification_subscriptions WHERE name = $1;

-- name: ListNotificationSubscriptions :many
SELECT * FROM notification_subscriptions ORDER BY name;

-- name: ListSubscriptionsForEvent :many
-- The subscriptions that covered an outbox event when it was committed, however late it is processed:
-- created by then, and active, interrupted, or superseded only after it (and not suspended, that is
-- abandoned, before it). Commit order is the commit epoch (migration 033), never a clock. A change
-- committed while a subscription was interrupted therefore goes to that subscription, is recorded as
-- suppressed and can be recovered; it never reaches the subscription that replaced it on its own. An
-- event without its outbox row selects nothing. The notifier decides again at every attempt; this only
-- selects whom to try.
SELECT s.id FROM outbox o JOIN notification_subscriptions s ON o.type = ANY(s.event_types) AND o.source = ANY(s.sources)
WHERE o.id = sqlc.arg(event_id)::uuid AND s.created_epoch <= o.commit_epoch
  AND (s.state IN ('active', 'needs_reapproval')
    OR s.state = 'superseded' AND (s.suspended_epoch IS NULL OR s.suspended_epoch > o.commit_epoch) AND EXISTS (
      SELECT 1 FROM notification_subscriptions n WHERE n.replaces = s.id AND n.created_epoch > o.commit_epoch))
ORDER BY s.id;

-- name: NewerChangeDelivered :one
-- Whether the subscription already delivered a later version of the record.
SELECT EXISTS (SELECT 1 FROM notification_deliveries WHERE subscription_id = $1 AND profile_id = $2 AND profile_version > $3
  AND outcome = 'delivered')::boolean AS newer;

-- name: SetNotificationSubscriptionState :one
-- Suspending advances the commit epoch, so changes are ordered against it by commit.
UPDATE notification_subscriptions SET state = $2, updated_at = now(),
  suspended_at = CASE WHEN $2 = 'suspended' THEN now() END,
  suspended_epoch = CASE WHEN $2 = 'suspended' THEN advance_commit_epoch() END
WHERE name = $1 RETURNING *;

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
-- verified_source marks a source grant made under a consent authority that is active now, not past its
-- review date and granted to the same source for the same purpose, with a signed receipt whose key is not
-- compromised, whose verification, if any, neither failed nor went unanswered, and whose authority has no
-- verification unanswered past its deadline (ADR 0017); under the verified trust policy, only once a
-- privacy admin reviewed its proof and recorded it verified.
SELECT e.subject_id, e.decision, e.received_at, e.actor_kind, e.quarantined, pv.version, pv.lawful_basis,
  (e.actor_kind = 'source' AND coalesce(a.state = 'active' AND a.review_by >= current_date AND a.source = e.source
    AND a.purpose_id = pv.purpose_id AND k.authority_id = a.id AND k.state <> 'compromised'
    -- A failed or overdue verification voids the grant, and any overdue verification of the authority
    -- stops all of its grants; under the verified policy a grant needs a reviewed, verified one.
    AND NOT EXISTS (SELECT 1 FROM consent_evidence_verifications v WHERE v.evidence_id = e.id
      AND (v.state = 'failed' OR v.state = 'requested' AND v.due_at < now()))
    AND NOT EXISTS (SELECT 1 FROM consent_evidence_verifications v WHERE v.authority_id = a.id AND v.state = 'requested' AND v.due_at < now())
    AND (a.trust_policy = 'attestation' OR EXISTS (SELECT 1 FROM consent_evidence_verifications v WHERE v.evidence_id = e.id AND v.state = 'verified')),
    false))::boolean AS verified_source
FROM consent_evidence e
JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
LEFT JOIN consent_authorities a ON a.id = e.authority_id
LEFT JOIN consent_authority_keys k ON k.id = e.receipt_key
WHERE e.subject_id = ANY(sqlc.arg(subject_ids)::uuid[]) AND pv.purpose_id = sqlc.arg(purpose_id)::uuid
LIMIT sqlc.arg(row_limit)::int;

-- name: ListRegisteredSources :many
SELECT name FROM sources WHERE name = ANY(sqlc.arg(names)::text[]);

-- name: ListSubscriptionsBoundToVersion :many
-- Subscriptions approved under a policy version that are still active, locked against concurrent change.
SELECT * FROM notification_subscriptions WHERE policy_version_id = $1 AND state = 'active' ORDER BY id FOR UPDATE;

-- name: ListSubscriptionsNeedingReapproval :many
-- Subscriptions of the purpose bound to a version that is no longer active: interrupted ones, and
-- suspended ones that were never resumed under their version.
SELECT s.* FROM notification_subscriptions s JOIN consent_policy_versions v ON v.id = s.policy_version_id
WHERE s.purpose_id = $1 AND (s.state = 'needs_reapproval' OR s.state = 'suspended' AND v.state <> 'active') ORDER BY s.id;

-- name: InterruptSubscriptions :execrows
UPDATE notification_subscriptions SET state = 'needs_reapproval', interrupted_at = now(), updated_at = now()
WHERE id = ANY(sqlc.arg(ids)::uuid[]) AND state = 'active';

-- name: SupersedeSubscription :execrows
UPDATE notification_subscriptions SET state = 'superseded', updated_at = now() WHERE id = $1 AND state IN ('needs_reapproval', 'suspended');

-- name: InsertSubscriptionAudit :exec
INSERT INTO notification_subscription_audit (id, subscription_id, action, policy_version_id, related_id, actor)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListSubscriptionAudit :many
SELECT * FROM notification_subscription_audit WHERE subscription_id = $1 ORDER BY recorded_at, id;

-- name: CountInterruptedSuppressions :many
-- Per subscription, the notifications suppressed because its policy version no longer authorized it.
SELECT subscription_id, count(*)::bigint AS suppressed, min(recorded_at)::timestamptz AS first_at, max(recorded_at)::timestamptz AS last_at
FROM notification_deliveries
WHERE outcome = 'suppressed' AND reason = ANY(sqlc.arg(reasons)::text[])
GROUP BY subscription_id;

-- name: GetSubscriptionByReplaces :one
SELECT * FROM notification_subscriptions WHERE replaces = $1;

-- name: InsertNotificationRecovery :one
INSERT INTO notification_recoveries (id, subscription_id, idempotency_key, requested_by, more_pending)
VALUES ($1, $2, $3, $4, $5) ON CONFLICT (subscription_id, idempotency_key) DO NOTHING RETURNING *;

-- name: GetNotificationRecoveryByKey :one
SELECT * FROM notification_recoveries WHERE subscription_id = $1 AND idempotency_key = $2;

-- name: GetNotificationRecovery :one
SELECT * FROM notification_recoveries WHERE id = $1 AND subscription_id = $2;

-- name: ListRecoveryCandidates :many
-- What a replaced subscription suppressed for lack of an approved policy version, that the replacing
-- subscription has neither recovered nor recorded yet, oldest first, with whether its event's outbox
-- reference still exists.
SELECT d.event_id, d.profile_id, d.profile_version, d.recorded_at, (o.id IS NOT NULL)::boolean AS history
FROM notification_deliveries d
LEFT JOIN outbox o ON o.id = d.event_id
WHERE d.subscription_id = sqlc.arg(replaced)::uuid AND d.outcome = 'suppressed' AND d.reason = ANY(sqlc.arg(reasons)::text[])
  AND NOT EXISTS (SELECT 1 FROM notification_recovery_items i WHERE i.subscription_id = sqlc.arg(subscription)::uuid AND i.event_id = d.event_id)
  AND NOT EXISTS (SELECT 1 FROM notification_deliveries n WHERE n.subscription_id = sqlc.arg(subscription)::uuid AND n.event_id = d.event_id)
ORDER BY d.recorded_at, d.event_id
LIMIT sqlc.arg(row_limit)::int;

-- name: LatestNotifiedVersions :many
-- Per profile, the newest version the subscription has delivered or queued for recovery.
SELECT profile_id, max(profile_version)::bigint AS version FROM (
  SELECT profile_id, profile_version FROM notification_deliveries WHERE subscription_id = sqlc.arg(subscription)::uuid AND outcome = 'delivered'
    AND profile_id = ANY(sqlc.arg(profile_ids)::uuid[])
  UNION ALL
  SELECT profile_id, profile_version FROM notification_recovery_items WHERE subscription_id = sqlc.arg(subscription)::uuid AND state IN ('queued', 'started')
    AND profile_id = ANY(sqlc.arg(profile_ids)::uuid[])
) v GROUP BY profile_id;

-- name: InsertRecoveryItems :execrows
INSERT INTO notification_recovery_items (subscription_id, event_id, recovery_id, profile_id, profile_version, state, reason)
SELECT sqlc.arg(subscription)::uuid, unnest(sqlc.arg(event_ids)::uuid[]), sqlc.arg(recovery)::uuid, unnest(sqlc.arg(profile_ids)::uuid[]),
  unnest(sqlc.arg(versions)::bigint[]), unnest(sqlc.arg(states)::text[]), nullif(unnest(sqlc.arg(reasons)::text[]), '')
ON CONFLICT (subscription_id, event_id) DO NOTHING;

-- name: SummarizeRecovery :many
SELECT i.state, coalesce(d.outcome, '')::text AS outcome, count(*)::bigint AS items
FROM notification_recovery_items i
LEFT JOIN notification_deliveries d ON d.event_id = i.event_id AND d.subscription_id = i.subscription_id
WHERE i.recovery_id = $1
GROUP BY 1, 2;

-- name: ListRecoveryItems :many
SELECT i.event_id, i.profile_id, i.profile_version, i.state, i.reason, i.started_at, d.outcome, d.reason AS outcome_reason
FROM notification_recovery_items i
LEFT JOIN notification_deliveries d ON d.event_id = i.event_id AND d.subscription_id = i.subscription_id
WHERE i.recovery_id = sqlc.arg(recovery)::uuid AND i.event_id > sqlc.arg(after)::uuid
ORDER BY i.event_id
LIMIT sqlc.arg(row_limit)::int;

-- name: ListQueuedRecoveryItems :many
-- Recovery items not yet started, with their event's outbox reference if it still exists, in random order
-- so that items that keep failing to start cannot hold back the rest. Order does not matter: an older
-- change is withheld once a later one was delivered.
SELECT i.subscription_id, i.event_id, o.type, o.source, o.version, o.occurred_at, i.profile_id
FROM notification_recovery_items i
LEFT JOIN outbox o ON o.id = i.event_id
WHERE i.state = 'queued'
ORDER BY random()
LIMIT sqlc.arg(row_limit)::int;

-- name: MarkRecoveryItem :execrows
UPDATE notification_recovery_items SET state = sqlc.arg(state)::text, reason = sqlc.narg(reason)::text,
  started_at = CASE WHEN sqlc.arg(state)::text = 'started' THEN now() END
WHERE subscription_id = sqlc.arg(subscription)::uuid AND event_id = sqlc.arg(event_id)::uuid AND state = 'queued';
