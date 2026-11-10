-- name: GetCustomerPreferences :one
SELECT * FROM customer_preferences WHERE anchor_profile_id = $1;

-- name: LockCustomerPreferences :one
SELECT * FROM customer_preferences WHERE anchor_profile_id = $1 FOR UPDATE;

-- name: UpsertCustomerPreferences :exec
INSERT INTO customer_preferences (anchor_profile_id, display_name, preferred_language, timezone, version)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (anchor_profile_id) DO UPDATE SET display_name = EXCLUDED.display_name, preferred_language = EXCLUDED.preferred_language,
  timezone = EXCLUDED.timezone, version = EXCLUDED.version, updated_at = now();

-- name: InsertPreferenceChange :exec
INSERT INTO customer_preference_changes (anchor_profile_id, version, fields) VALUES ($1, $2, $3);

-- name: ListPreferenceChanges :many
SELECT version, fields, changed_at FROM customer_preference_changes WHERE anchor_profile_id = $1 ORDER BY version DESC LIMIT 50;
