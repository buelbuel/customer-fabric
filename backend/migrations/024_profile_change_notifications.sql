-- +goose Up
-- Outcomes of the profile-change workflow (#64, ADR 0014): one row per outbox event, written once. The
-- row is also the durable de-duplication record: an event redelivered after Temporal forgot its workflow
-- is not sent again. It holds identifiers and fixed reason codes only, never values.
CREATE TABLE profile_change_notifications (
 event_id uuid PRIMARY KEY,
 profile_id uuid NOT NULL,
 profile_version bigint NOT NULL CHECK (profile_version > 0),
 destination text NOT NULL,
 outcome text NOT NULL CHECK (outcome IN ('delivered','suppressed','failed')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 status_code integer,
 recorded_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- Drops notification history. Use only in a disposable database.
DROP TABLE profile_change_notifications;
