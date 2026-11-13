-- +goose Up
-- Retention for eventing and notification data (#28, #64; ADR 0014). Each category has a configured
-- maximum age and nothing defaults to indefinite. Every purge that removed rows leaves a receipt: the
-- category, the horizon it applied and how many rows it removed, never which.
CREATE TABLE retention_purges (
 id uuid PRIMARY KEY,
 category text NOT NULL CHECK (category IN ('outbox','notification_outcomes','withdrawal_notices','notification_recoveries')),
 horizon timestamptz NOT NULL,
 purged bigint NOT NULL CHECK (purged > 0),
 ran_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX retention_purges_category ON retention_purges(category, ran_at);

CREATE INDEX outbox_retention ON outbox(occurred_at);
CREATE INDEX notification_deliveries_retention ON notification_deliveries(recorded_at);
CREATE INDEX notification_recoveries_retention ON notification_recoveries(requested_at);
CREATE INDEX notification_withdrawals_retention ON notification_withdrawals(settled_at) WHERE state IN ('delivered','cancelled');

-- Withdrawal attempts stay append-only: never updated, and deleted only by a retention purge, which
-- declares itself for its own transaction.
DROP TRIGGER notification_withdrawal_attempt_immutable ON notification_withdrawal_attempts;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notification_withdrawal_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP = 'DELETE' AND current_setting('customer_fabric.retention_purge', true) = 'on' THEN
    RETURN OLD;
  END IF;
  RAISE EXCEPTION 'withdrawal attempts are append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER notification_withdrawal_attempt_immutable BEFORE UPDATE OR DELETE ON notification_withdrawal_attempts
  FOR EACH ROW EXECUTE FUNCTION notification_withdrawal_attempt_immutable();

-- +goose Down
DROP TRIGGER notification_withdrawal_attempt_immutable ON notification_withdrawal_attempts;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION notification_withdrawal_attempt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'withdrawal attempts are append-only';
END $$;
-- +goose StatementEnd
CREATE TRIGGER notification_withdrawal_attempt_immutable BEFORE UPDATE OR DELETE ON notification_withdrawal_attempts
  FOR EACH ROW EXECUTE FUNCTION notification_withdrawal_attempt_immutable();
DROP INDEX notification_withdrawals_retention;
DROP INDEX notification_recoveries_retention;
DROP INDEX notification_deliveries_retention;
DROP INDEX outbox_retention;
DROP TABLE retention_purges;
