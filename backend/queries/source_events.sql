-- name: LockSourceEpoch :one
-- A share lock, so an epoch advance waits for in-flight events and they cannot commit after it.
SELECT epoch FROM sources WHERE name=$1 FOR SHARE;

-- name: AdvanceSourceEpoch :one
UPDATE sources SET epoch = epoch + 1 WHERE name=$1 RETURNING epoch;

-- name: ResolveSourceRecord :one
-- Scoped to the credential's source and to a profile that source created, so an event can never
-- reach another source's record or a customer anchor.
SELECT e.profile_id, e.record_epoch
FROM external_identifiers e
JOIN profiles p ON p.id = e.profile_id AND p.creator_source = e.source
WHERE e.source=$1 AND e.namespace=$2 AND e.value=$3;

-- name: GetSourceEvent :one
SELECT digest, profile_id, outcome, mutation_id, profile_version FROM source_events WHERE source=$1 AND event_id=$2;

-- name: GetSourceRecordWatermark :one
SELECT source_epoch, sequence FROM source_record_watermarks WHERE record_epoch=$1;

-- name: SetSourceRecordWatermark :exec
INSERT INTO source_record_watermarks(record_epoch, source_epoch, sequence) VALUES($1,$2,$3)
ON CONFLICT(record_epoch) DO UPDATE SET source_epoch=EXCLUDED.source_epoch, sequence=EXCLUDED.sequence, updated_at=clock_timestamp();

-- name: InsertSourceEvent :exec
INSERT INTO source_events(source, event_id, digest, record_epoch, profile_id, source_epoch, sequence, occurred_at, outcome, mutation_id, profile_version)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11);
