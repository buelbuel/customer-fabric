-- +goose Up
-- Operator redrive of failed deliveries (#38, slice 3 of #123). Each redrive grants the delivery a fresh
-- bounded attempt budget; it is dispatched through the same gates, so it can never override suppression.
-- attempt_base is the attempt count when the delivery was last redriven; it may make MaxAttempts more.
ALTER TABLE activation_deliveries ADD COLUMN redrives integer NOT NULL DEFAULT 0 CHECK (redrives BETWEEN 0 AND 10),
  ADD COLUMN attempt_base integer NOT NULL DEFAULT 0 CHECK (attempt_base >= 0);

-- +goose Down
-- Drops redrive history. Use only in a disposable database.
ALTER TABLE activation_deliveries DROP COLUMN redrives, DROP COLUMN attempt_base;
