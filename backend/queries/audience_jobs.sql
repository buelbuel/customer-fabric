-- name: InsertAudienceJob :execrows
INSERT INTO audience_jobs (id, kind, segment_name, segment_revision, purpose_id, destination, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (idempotency_key) DO NOTHING;

-- name: GetAudienceJob :one
SELECT sqlc.embed(j), coalesce(p.evaluated, 0)::int AS evaluated FROM audience_jobs j
LEFT JOIN audience_job_progress p ON p.job_id = j.id AND p.attempt = j.attempts WHERE j.id = $1;

-- name: GetAudienceJobByKey :one
SELECT sqlc.embed(j), coalesce(p.evaluated, 0)::int AS evaluated FROM audience_jobs j
LEFT JOIN audience_job_progress p ON p.job_id = j.id AND p.attempt = j.attempts WHERE j.idempotency_key = $1;

-- name: ListAudienceJobs :many
SELECT sqlc.embed(j), coalesce(p.evaluated, 0)::int AS evaluated FROM audience_jobs j
LEFT JOIN audience_job_progress p ON p.job_id = j.id AND p.attempt = j.attempts
ORDER BY j.created_at DESC, j.id LIMIT 100;

-- name: GetAudienceJobState :one
SELECT state FROM audience_jobs WHERE id = $1;

-- name: CountPendingAudienceJobs :one
SELECT count(*)::bigint FROM audience_jobs WHERE state IN ('queued','running');

-- name: ListPendingAudienceJobs :many
-- Jobs to try, longest-waiting first. A running job is listed too: its worker may have stopped, which
-- only the job's advisory lock can tell.
SELECT id FROM audience_jobs WHERE state IN ('queued','running') ORDER BY updated_at, id LIMIT sqlc.arg(row_limit)::int;

-- name: TryAudienceJobLock :one
-- A session lock held for one attempt. It is released when the worker's connection ends, so a stopped
-- worker never leaves a job claimed.
SELECT pg_try_advisory_lock(hashtextextended('audience-job:' || sqlc.arg(id)::uuid::text, 0));

-- name: ReleaseAudienceJobLock :one
SELECT pg_advisory_unlock(hashtextextended('audience-job:' || sqlc.arg(id)::uuid::text, 0));

-- name: StartAudienceJobAttempt :one
UPDATE audience_jobs SET state = 'running', attempts = attempts + 1, updated_at = now()
WHERE id = $1 AND state IN ('queued','running') RETURNING *;

-- name: ReportAudienceJobProgress :exec
INSERT INTO audience_job_progress (job_id, attempt, evaluated) VALUES ($1, $2, $3)
ON CONFLICT (job_id) DO UPDATE SET attempt = excluded.attempt, evaluated = excluded.evaluated, updated_at = now();

-- name: CompleteAudienceJob :execrows
-- Runs inside the evaluating transaction. It applies only while the job is still running, and under
-- REPEATABLE READ it fails if the job was cancelled after the evaluation's snapshot was taken.
UPDATE audience_jobs SET state = 'completed', result_id = sqlc.arg(result_id)::uuid, evaluated_at = sqlc.arg(evaluated_at)::timestamptz,
  population = sqlc.arg(population)::int, members = sqlc.arg(members)::int,
  excluded_unavailable = sqlc.arg(excluded_unavailable)::int, excluded_consent = sqlc.arg(excluded_consent)::int,
  excluded_criteria = sqlc.arg(excluded_criteria)::int, finished_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND state = 'running';

-- name: RequeueAudienceJob :execrows
-- Returns a job whose attempt failed to the queue, behind the jobs already waiting.
UPDATE audience_jobs SET state = 'queued', updated_at = now() WHERE id = $1 AND state = 'running';

-- name: EndAudienceJob :execrows
-- Ends a pending job as failed or cancelled with a reason code. A finished job is left as it is.
UPDATE audience_jobs SET state = sqlc.arg(state)::text, reason = sqlc.arg(reason)::text, finished_at = now(), updated_at = now()
WHERE id = sqlc.arg(id)::uuid AND state IN ('queued','running');

-- name: ListSnapshotMembers :many
-- One page of a snapshot's members, by anchor keyset.
SELECT anchor_profile_id FROM segment_snapshot_members
WHERE snapshot_id = sqlc.arg(snapshot_id)::uuid AND anchor_profile_id > sqlc.arg(after)::uuid
ORDER BY anchor_profile_id LIMIT sqlc.arg(row_limit)::int;
