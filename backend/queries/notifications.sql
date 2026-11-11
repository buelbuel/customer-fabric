-- name: GetProfileLifecycleState :one
SELECT lifecycle_state, restricted, version FROM profiles WHERE id = $1;

-- name: GetProfileChangeNotification :one
SELECT * FROM profile_change_notifications WHERE event_id = $1;

-- name: RecordProfileChangeNotification :execrows
-- Written once per event; a later attempt to record the same event changes nothing.
INSERT INTO profile_change_notifications (event_id, profile_id, profile_version, destination, outcome, reason, status_code)
VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT (event_id) DO NOTHING;
