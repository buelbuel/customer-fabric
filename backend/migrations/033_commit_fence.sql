-- +goose Up
-- Commit fence (ADR 0016). Which subscriptions cover a change depends on whether the change committed
-- before or after a subscription was created, replaced or suspended. Transaction start times cannot
-- decide that, so both sides are ordered by one commit epoch instead:
--   - every transaction that writes the outbox holds the fence shared from its start to its commit, and
--     stamps its rows with the epoch it sees;
--   - creating or suspending a subscription takes the fence exclusively and advances the epoch, which it
--     records. It waits for the writes in flight, and new writes wait for it to commit.
-- A change therefore carries an epoch below a subscription change's exactly when it committed first.
CREATE TABLE commit_epoch (
 singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
 epoch bigint NOT NULL CHECK (epoch >= 0)
);

-- +goose StatementBegin
-- The current epoch, for a row written inside the fence; anything else is refused.
CREATE FUNCTION fenced_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  k bigint := hashtextextended('commit-fence', 0);
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
                 AND classid = ((k >> 32) & 4294967295)::oid AND objid = (k & 4294967295)::oid AND objsubid = 1) THEN
    RAISE EXCEPTION 'outbox write outside the commit fence';
  END IF;
  RETURN (SELECT epoch FROM commit_epoch);
END $$;

-- Takes the fence exclusively and returns the next epoch.
CREATE FUNCTION advance_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  advanced bigint;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('commit-fence', 0));
  UPDATE commit_epoch SET epoch = epoch + 1 RETURNING epoch INTO advanced;
  RETURN advanced;
END $$;
-- +goose StatementEnd

-- Existing rows keep the decisions their times gave: each distinct subscription creation or suspension
-- time becomes an epoch, and a change is stamped with the number of those times at or before it.
CREATE TEMPORARY TABLE subscription_boundaries ON COMMIT DROP AS
SELECT t, row_number() OVER (ORDER BY t) AS n FROM (
  SELECT created_at AS t FROM notification_subscriptions
  UNION SELECT suspended_at FROM notification_subscriptions WHERE suspended_at IS NOT NULL) b;
INSERT INTO commit_epoch (epoch) SELECT count(*) FROM subscription_boundaries;

ALTER TABLE notification_subscriptions ADD COLUMN created_epoch bigint, ADD COLUMN suspended_epoch bigint;
UPDATE notification_subscriptions s SET
  created_epoch = (SELECT n FROM subscription_boundaries WHERE t = s.created_at),
  suspended_epoch = (SELECT n FROM subscription_boundaries WHERE t = s.suspended_at);
ALTER TABLE notification_subscriptions
  ALTER COLUMN created_epoch SET NOT NULL,
  ALTER COLUMN created_epoch SET DEFAULT advance_commit_epoch(),
  ADD CONSTRAINT notification_subscriptions_suspended_epoch CHECK ((suspended_at IS NULL) = (suspended_epoch IS NULL));

ALTER TABLE outbox ADD COLUMN commit_epoch bigint;
UPDATE outbox o SET commit_epoch = (SELECT count(*) FROM subscription_boundaries WHERE t <= o.occurred_at);
ALTER TABLE outbox ALTER COLUMN commit_epoch SET NOT NULL, ALTER COLUMN commit_epoch SET DEFAULT fenced_commit_epoch();

-- +goose Down
ALTER TABLE outbox DROP COLUMN commit_epoch;
ALTER TABLE notification_subscriptions DROP CONSTRAINT notification_subscriptions_suspended_epoch,
  DROP COLUMN suspended_epoch, DROP COLUMN created_epoch;
DROP FUNCTION advance_commit_epoch();
DROP FUNCTION fenced_commit_epoch();
DROP TABLE commit_epoch;
