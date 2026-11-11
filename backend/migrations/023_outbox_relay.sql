-- +goose Up
-- Outbox relay checkpoint (#64, ADR 0013). The relay publishes committed outbox rows to Kafka and then
-- records when it did; a crash between the broker's acknowledgement and this checkpoint republishes the
-- row, so consumers deduplicate by event ID. Rows are kept after relaying, as before.
ALTER TABLE outbox ADD COLUMN relayed_at timestamptz;
CREATE INDEX outbox_unrelayed ON outbox(occurred_at, id) WHERE relayed_at IS NULL;

-- +goose Down
-- Destructive: only use on disposable databases. Production recovery is roll-forward.
DROP INDEX outbox_unrelayed;
ALTER TABLE outbox DROP COLUMN relayed_at;
