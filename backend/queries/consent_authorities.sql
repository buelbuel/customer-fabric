-- name: InsertConsentAuthority :one
INSERT INTO consent_authorities (id, source, purpose_id, controller, collection_methods, approved_by, arrangement_reference,
  vetting_reference, review_by, trust_policy)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING *;

-- name: ListConsentAuthorities :many
SELECT * FROM consent_authorities ORDER BY approved_at DESC, id LIMIT 200;

-- name: GetActiveConsentAuthority :one
-- The source's authority for the purpose that is active and not past its review date.
-- Shared-locked, so a grant appended under it serializes with suspending, revoking or changing its keys.
-- An authority with a verification unanswered past its deadline accepts nothing until it is reviewed.
SELECT * FROM consent_authorities a WHERE a.source = $1 AND a.purpose_id = $2 AND a.state = 'active' AND a.review_by >= current_date
  AND NOT EXISTS (SELECT 1 FROM consent_evidence_verifications v WHERE v.authority_id = a.id AND v.state = 'requested' AND v.due_at < now())
FOR SHARE;

-- name: RevokeConsentAuthority :one
UPDATE consent_authorities SET state = 'revoked', revoked_at = now(), revoked_by = $2 WHERE id = $1 AND state <> 'revoked' RETURNING *;

-- name: SuspendConsentAuthority :one
UPDATE consent_authorities SET state = 'suspended' WHERE id = $1 AND state = 'active' RETURNING *;

-- name: RenewConsentAuthority :one
-- Records a new review of the source and resumes the authority.
UPDATE consent_authorities SET state = 'active', vetting_reference = $2, arrangement_reference = $3, review_by = $4, trust_policy = $5
WHERE id = $1 AND state <> 'revoked' RETURNING *;

-- name: LockConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1 FOR UPDATE;

-- name: InsertAuthorityKey :one
-- Refused, returning nothing, when the key_id is taken on the authority or the public key was ever registered.
INSERT INTO consent_authority_keys (id, authority_id, key_id, public_key, added_by) VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING RETURNING *;

-- name: ListAuthorityKeys :many
SELECT * FROM consent_authority_keys WHERE authority_id = $1 ORDER BY added_at, key_id;

-- name: GetAuthorityKey :one
SELECT * FROM consent_authority_keys WHERE authority_id = $1 AND key_id = $2 FOR SHARE;

-- name: SetAuthorityKeyState :one
-- Retires an active key, or marks an active or retired key compromised; compromise is final.
UPDATE consent_authority_keys SET state = sqlc.arg(state)::text, changed_at = now()
WHERE authority_id = sqlc.arg(authority_id)::uuid AND key_id = sqlc.arg(key_id)::text
  AND (state = 'active' OR state = 'retired' AND sqlc.arg(state)::text = 'compromised')
RETURNING *;

-- name: InsertAuthorityAudit :exec
INSERT INTO consent_authority_audit (id, authority_id, action, key_id, reference, arrangement_reference, review_by, trust_policy, actor)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListAuthorityAudit :many
SELECT * FROM consent_authority_audit WHERE authority_id = $1 ORDER BY recorded_at, id;

-- name: GetConsentAuthority :one
SELECT * FROM consent_authorities WHERE id = $1;

-- name: LatestProtectiveDecision :one
-- When the subject last withdrew, denied or objected for the purpose, under any version; the Unix epoch if never.
SELECT coalesce(max(e.received_at), 'epoch'::timestamptz)::timestamptz AS received_at
FROM consent_evidence e JOIN consent_policy_versions pv ON pv.id = e.policy_version_id
WHERE e.subject_id = $1 AND pv.purpose_id = $2 AND e.decision <> 'grant';

-- name: SuspendAuthorityAutomatically :execrows
UPDATE consent_authorities SET state = 'suspended' WHERE id = $1 AND state = 'active';

-- name: GetSourceGrant :one
-- A source's grant, with the record identifier its subject is anchored on, for a verification request.
SELECT e.id, e.authority_id, e.source, e.decision, e.actor_kind FROM consent_evidence e WHERE e.id = $1;

-- name: InsertVerification :one
INSERT INTO consent_evidence_verifications (id, evidence_id, authority_id, reason, requested_by, due_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING RETURNING *;

-- name: ListEvidenceVerifications :many
SELECT * FROM consent_evidence_verifications WHERE evidence_id = $1 ORDER BY requested_at, id;

-- name: SampleUnverifiedGrants :many
-- Grants under the authority that were never asked for verification, at random.
SELECT e.id FROM consent_evidence e
WHERE e.authority_id = $1 AND e.actor_kind = 'source' AND e.decision = 'grant'
  AND NOT EXISTS (SELECT 1 FROM consent_evidence_verifications v WHERE v.evidence_id = e.id)
ORDER BY random() LIMIT sqlc.arg(row_limit)::int;

-- name: ListOpenVerificationsForSource :many
-- Verifications the source has to answer: the grant's own receipt reference and the record it names.
SELECT v.id, v.reason, v.requested_at, v.due_at, e.proof_reference, e.policy_version_id, x.namespace, x.value
FROM consent_evidence_verifications v
JOIN consent_evidence e ON e.id = v.evidence_id
JOIN consent_subjects s ON s.id = e.subject_id
LEFT JOIN external_identifiers x ON x.record_epoch = s.anchor_record_epoch
WHERE e.source = $1 AND v.state = 'requested' AND v.due_at >= now()
ORDER BY v.due_at, v.id LIMIT 200;

-- name: LockVerificationForSource :one
-- An open verification of one of the source's grants, locked, with everything its proof must match.
SELECT v.id, v.authority_id, v.due_at, e.policy_version_id, e.decision, e.occurred_at, e.proof_channel, e.proof_reference,
  e.notice_reference, e.proof_digest, s.anchor_record_epoch, x.namespace, x.value, a.controller
FROM consent_evidence_verifications v
JOIN consent_evidence e ON e.id = v.evidence_id
JOIN consent_subjects s ON s.id = e.subject_id
JOIN consent_authorities a ON a.id = v.authority_id
LEFT JOIN external_identifiers x ON x.record_epoch = s.anchor_record_epoch
WHERE v.id = $1 AND e.source = $2 AND v.state = 'requested'
FOR UPDATE OF v;

-- name: DecideVerification :one
-- Records the automatic check of a proof document: checked (consistent) or failed.
UPDATE consent_evidence_verifications SET state = $2, result = $3, document_digest = $4, decided_at = now()
WHERE id = $1 AND state = 'requested' RETURNING *;

-- name: ReviewVerification :one
-- A privacy admin's review of a checked proof, or the closing of an overdue request as failed.
UPDATE consent_evidence_verifications SET state = sqlc.arg(state)::text, reviewed_by = sqlc.arg(reviewed_by)::text,
  review_reference = sqlc.arg(review_reference)::text, reviewed_at = now(), decided_at = coalesce(decided_at, now()),
  result = coalesce(result, 'unanswered')
WHERE id = sqlc.arg(id)::uuid AND evidence_id = sqlc.arg(evidence_id)::uuid
  AND (state = 'checked' OR state = 'requested' AND due_at < now() AND sqlc.arg(state)::text = 'failed')
RETURNING *;

-- name: GetPolicyVerification :one
SELECT id FROM consent_evidence_verifications WHERE evidence_id = $1 AND reason = 'policy' ORDER BY requested_at LIMIT 1;

-- name: CountSourceConsentRequest :one
-- Counts one consent decision against the source's current minute and returns the minute's total.
INSERT INTO source_consent_rate (source, window_start, requests) VALUES ($1, date_trunc('minute', now()), 1)
ON CONFLICT (source, window_start) DO UPDATE SET requests = source_consent_rate.requests + 1
RETURNING requests;

-- name: PruneSourceConsentRate :exec
DELETE FROM source_consent_rate WHERE window_start < now() - interval '1 hour';
