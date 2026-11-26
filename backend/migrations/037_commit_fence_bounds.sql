-- +goose Up
-- Bounds on the commit fence (ADR 0016).
--   - The fence is keyed by schema, not only by database: advisory locks are database-wide, so one
--     constant key made every schema of a shared database wait for the others' subscription changes.
--   - A transaction can be given finite bounds when it starts: a lock wait that ends in an error, and a
--     deadline the server enforces however the client behaves. A write holds the fence only inside them.
--   - Suspending a subscription blocks its delivery at once, before it waits for the fence.
-- +goose StatementBegin
DO $$ BEGIN
  IF current_setting('server_version_num')::integer < 170000 THEN
    RAISE EXCEPTION 'the commit fence deadline needs PostgreSQL 17 or later (transaction_timeout)';
  END IF;
END $$;

CREATE FUNCTION commit_fence_key() RETURNS bigint LANGUAGE sql STABLE AS $$
  SELECT hashtextextended('commit-fence:' || coalesce(current_schema(), ''), 0)
$$;

-- Bounds the rest of the transaction: no lock wait lasts longer than lock_wait_ms, and the transaction
-- is ended by the server after transaction_ms. Both are required, so nothing here can wait for ever.
CREATE FUNCTION bound_transaction(lock_wait_ms integer, transaction_ms integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  IF lock_wait_ms IS NULL OR lock_wait_ms < 1 OR transaction_ms IS NULL OR transaction_ms < 1 THEN
    RAISE EXCEPTION 'a transaction needs finite bounds';
  END IF;
  PERFORM set_config('lock_timeout', lock_wait_ms::text, true), set_config('transaction_timeout', transaction_ms::text, true);
END $$;

-- Bounds the transaction, then holds the fence shared to its end. The wait for the fence is the lock wait.
CREATE FUNCTION enter_commit_fence(lock_wait_ms integer, transaction_ms integer) RETURNS void LANGUAGE plpgsql AS $$
BEGIN
  PERFORM bound_transaction(lock_wait_ms, transaction_ms);
  PERFORM pg_advisory_xact_lock_shared(commit_fence_key());
END $$;

CREATE OR REPLACE FUNCTION fenced_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  k bigint := commit_fence_key();
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
                 AND classid = ((k >> 32) & 4294967295)::oid AND objid = (k & 4294967295)::oid AND objsubid = 1) THEN
    RAISE EXCEPTION 'outbox write outside the commit fence';
  END IF;
  RETURN (SELECT epoch FROM commit_epoch);
END $$;

CREATE OR REPLACE FUNCTION advance_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  advanced bigint;
BEGIN
  PERFORM pg_advisory_xact_lock(commit_fence_key());
  UPDATE commit_epoch SET epoch = epoch + 1 RETURNING epoch INTO advanced;
  RETURN advanced;
END $$;
-- +goose StatementEnd

-- Set from the moment a suspension is requested until it completes (or the subscription is resumed): the
-- notifier sends nothing to a subscription with it set, whatever the fence is doing.
ALTER TABLE notification_subscriptions ADD COLUMN delivery_halted_at timestamptz;

-- +goose Down
ALTER TABLE notification_subscriptions DROP COLUMN delivery_halted_at;
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION advance_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  advanced bigint;
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended('commit-fence', 0));
  UPDATE commit_epoch SET epoch = epoch + 1 RETURNING epoch INTO advanced;
  RETURN advanced;
END $$;

CREATE OR REPLACE FUNCTION fenced_commit_epoch() RETURNS bigint LANGUAGE plpgsql AS $$
DECLARE
  k bigint := hashtextextended('commit-fence', 0);
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND pid = pg_backend_pid() AND granted
                 AND classid = ((k >> 32) & 4294967295)::oid AND objid = (k & 4294967295)::oid AND objsubid = 1) THEN
    RAISE EXCEPTION 'outbox write outside the commit fence';
  END IF;
  RETURN (SELECT epoch FROM commit_epoch);
END $$;
-- +goose StatementEnd
DROP FUNCTION enter_commit_fence(integer, integer);
DROP FUNCTION bound_transaction(integer, integer);
DROP FUNCTION commit_fence_key();
