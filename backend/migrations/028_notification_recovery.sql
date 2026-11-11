-- +goose Up
-- Policy changes and notification subscriptions (#152, ADR 0016). A subscription stays bound to the
-- policy version it was approved under. Retiring that version interrupts it: it needs reapproval, and
-- its events are recorded as suppressed until then. Reapproval is a new subscription under the new
-- version that replaces the old one, which is kept, superseded. Nothing missed is sent unless an
-- operator requests a recovery, and every recovered notification is authorized again before it is sent.
ALTER TABLE notification_subscriptions DROP CONSTRAINT notification_subscriptions_state_check;
ALTER TABLE notification_subscriptions
  ADD CONSTRAINT notification_subscriptions_state_check CHECK (state IN ('active','suspended','needs_reapproval','superseded')),
  ADD COLUMN interrupted_at timestamptz,
  ADD COLUMN replaces uuid UNIQUE REFERENCES notification_subscriptions(id),
  ADD CONSTRAINT notification_subscriptions_interrupted CHECK (state <> 'needs_reapproval' OR interrupted_at IS NOT NULL);

-- Append-only history of what changed a subscription's authorization, and who did it.
CREATE TABLE notification_subscription_audit (
 id uuid PRIMARY KEY,
 subscription_id uuid NOT NULL REFERENCES notification_subscriptions(id),
 action text NOT NULL CHECK (action IN ('interrupted','reapproved','superseded','recovery_requested')),
 policy_version_id uuid REFERENCES consent_policy_versions(id),
 related_id uuid,
 actor text NOT NULL CHECK (char_length(actor) BETWEEN 1 AND 128),
 recorded_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_subscription_audit_subscription ON notification_subscription_audit(subscription_id, recorded_at);

-- Subscriptions approved under a version that is no longer active were already authorizing nothing.
-- When that happened is not recorded, so the interruption is dated and audited at this migration.
WITH interrupted AS (
  UPDATE notification_subscriptions s SET state = 'needs_reapproval', interrupted_at = now(), updated_at = now()
  WHERE s.state = 'active' AND NOT EXISTS (
    SELECT 1 FROM consent_policy_versions v WHERE v.id = s.policy_version_id AND v.state = 'active')
  RETURNING s.id, s.policy_version_id)
INSERT INTO notification_subscription_audit (id, subscription_id, action, policy_version_id, actor)
SELECT gen_random_uuid(), id, 'interrupted', policy_version_id, 'migration' FROM interrupted;

-- Recovery and every attempt look up whether a later version of a record was already delivered.
CREATE INDEX notification_deliveries_profile ON notification_deliveries(subscription_id, profile_id, profile_version);

-- An operator's request to send, under a reapproved subscription, what its predecessor suppressed while
-- it needed reapproval. Requests are idempotent per subscription and key.
CREATE TABLE notification_recoveries (
 id uuid PRIMARY KEY,
 subscription_id uuid NOT NULL REFERENCES notification_subscriptions(id),
 idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9_-]{16,128}$'),
 requested_by text NOT NULL CHECK (char_length(requested_by) BETWEEN 1 AND 128),
 requested_at timestamptz NOT NULL DEFAULT now(),
 more_pending boolean NOT NULL,
 UNIQUE (subscription_id, idempotency_key)
);

-- One item per event and subscription, across all recoveries, so an event is never recovered twice.
-- Its delivery outcome is the subscription's row in notification_deliveries. Items hold references only.
CREATE TABLE notification_recovery_items (
 subscription_id uuid NOT NULL REFERENCES notification_subscriptions(id),
 event_id uuid NOT NULL,
 recovery_id uuid NOT NULL REFERENCES notification_recoveries(id),
 profile_id uuid NOT NULL,
 profile_version bigint NOT NULL CHECK (profile_version > 0),
 state text NOT NULL CHECK (state IN ('queued','started','superseded','unrecoverable')),
 reason text CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 started_at timestamptz,
 PRIMARY KEY (subscription_id, event_id),
 CHECK ((state IN ('superseded','unrecoverable')) = (reason IS NOT NULL))
);
CREATE INDEX notification_recovery_items_recovery ON notification_recovery_items(recovery_id, event_id);
CREATE INDEX notification_recovery_items_queued ON notification_recovery_items(recovery_id) WHERE state = 'queued';

-- +goose Down
-- Drops recovery history and the interruption record. Use only in a disposable database.
DROP TABLE notification_recovery_items;
DROP TABLE notification_recoveries;
DROP INDEX notification_deliveries_profile;
DROP TABLE notification_subscription_audit;
UPDATE notification_subscriptions SET state = 'suspended' WHERE state IN ('needs_reapproval','superseded');
ALTER TABLE notification_subscriptions DROP CONSTRAINT notification_subscriptions_interrupted,
  DROP COLUMN replaces, DROP COLUMN interrupted_at, DROP CONSTRAINT notification_subscriptions_state_check;
ALTER TABLE notification_subscriptions ADD CONSTRAINT notification_subscriptions_state_check CHECK (state IN ('active','suspended'));
