-- +goose Up
-- Withdrawal notices (ADR 0014). A subscription holds a record once it delivered a change of it: the
-- recipient may keep it. When the record stops being authorized for the subscription's purpose, its
-- destination is sent one signed notice to stop using it, and each attempt is recorded. A notice covers
-- the newest version delivered before it; a later delivery holds the record again.
CREATE TABLE notification_withdrawals (
 id uuid PRIMARY KEY,
 subscription_id uuid NOT NULL REFERENCES notification_subscriptions(id),
 profile_id uuid NOT NULL REFERENCES profiles(id),
 held_version bigint NOT NULL CHECK (held_version > 0),
 reason text NOT NULL CHECK (reason ~ '^[a-z_]{1,64}$'),
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','delivered','failed','cancelled')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 attempt_base integer NOT NULL DEFAULT 0 CHECK (attempt_base >= 0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 lease_until timestamptz,
 created_at timestamptz NOT NULL DEFAULT now(),
 settled_at timestamptz,
 UNIQUE (subscription_id, profile_id, held_version)
);
CREATE INDEX notification_withdrawals_due ON notification_withdrawals(next_attempt_at) WHERE state = 'pending';
CREATE INDEX notification_withdrawals_subscription ON notification_withdrawals(subscription_id, created_at);

-- Every attempt and decision on a notice, append-only: outcome, reason code and the status code
-- received, never a payload.
CREATE TABLE notification_withdrawal_attempts (
 withdrawal_id uuid NOT NULL REFERENCES notification_withdrawals(id),
 attempt integer NOT NULL CHECK (attempt >= 0),
 outcome text NOT NULL CHECK (outcome IN ('delivered','retry','failed','cancelled')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_]{1,64}$'),
 status_code integer CHECK (status_code BETWEEN 100 AND 599),
 recorded_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (withdrawal_id, attempt, outcome)
);
-- +goose StatementBegin
CREATE FUNCTION notification_withdrawal_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'withdrawal attempts are append-only';
END $$;
CREATE TRIGGER notification_withdrawal_attempt_immutable BEFORE UPDATE OR DELETE ON notification_withdrawal_attempts
  FOR EACH ROW EXECUTE FUNCTION notification_withdrawal_attempt_immutable();
-- +goose StatementEnd

-- Records whose authorization may have changed, queued in the transaction that changed it. A sweep over
-- every held record covers what no write announces, such as an overdue grant verification.
CREATE TABLE notification_review_queue (
 profile_id uuid PRIMARY KEY REFERENCES profiles(id),
 enqueued_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notification_deliveries_held ON notification_deliveries(profile_id, subscription_id) WHERE outcome = 'delivered';

-- +goose Down
DROP INDEX notification_deliveries_held;
DROP TABLE notification_review_queue;
DROP TABLE notification_withdrawal_attempts;
DROP FUNCTION notification_withdrawal_attempt_immutable();
DROP TABLE notification_withdrawals;
