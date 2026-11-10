-- +goose Up
-- Consent-gated activation to test webhooks (#37, #44, Epic 4 of #117). An activation queues one delivery
-- per segment member in the same transaction that evaluated membership; a dispatcher re-checks each
-- delivery immediately before sending. No Kafka or Temporal: PostgreSQL holds the queue.
CREATE TABLE destinations (
 name text PRIMARY KEY CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
 url text NOT NULL CHECK (url ~ '^https?://' AND octet_length(url) <= 2048),
 -- The HMAC key that signs deliveries. Returned once at creation and never again.
 signing_secret bytea NOT NULL CHECK (octet_length(signing_secret) = 32),
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE activations (
 id uuid PRIMARY KEY,
 segment_name text NOT NULL,
 segment_revision integer NOT NULL,
 destination text NOT NULL REFERENCES destinations(name),
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 created_at timestamptz NOT NULL DEFAULT now(),
 FOREIGN KEY (segment_name, segment_revision) REFERENCES segment_definitions(name, revision)
);

-- reason is a fixed code, never customer data. attempts counts claims, so a crashed send still counts.
CREATE TABLE activation_deliveries (
 id uuid PRIMARY KEY,
 activation_id uuid NOT NULL REFERENCES activations(id),
 anchor_profile_id uuid NOT NULL REFERENCES profiles(id),
 state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','delivered','suppressed','failed')),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 next_attempt_at timestamptz NOT NULL DEFAULT now(),
 reason text CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 updated_at timestamptz NOT NULL DEFAULT now(),
 UNIQUE (activation_id, anchor_profile_id)
);
CREATE INDEX activation_deliveries_due ON activation_deliveries(next_attempt_at) WHERE state = 'pending';

-- One receipt per attempt: what was decided and the receiver's status code, never the payload.
CREATE TABLE delivery_receipts (
 delivery_id uuid NOT NULL REFERENCES activation_deliveries(id),
 attempt integer NOT NULL CHECK (attempt > 0),
 outcome text NOT NULL CHECK (outcome IN ('delivered','suppressed','retry','failed')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 status_code integer CHECK (status_code BETWEEN 100 AND 599),
 at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (delivery_id, attempt)
);
-- +goose StatementBegin
CREATE FUNCTION delivery_receipt_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'delivery receipts are append-only';
END $$;
CREATE TRIGGER delivery_receipt_immutable BEFORE UPDATE OR DELETE ON delivery_receipts
  FOR EACH ROW EXECUTE FUNCTION delivery_receipt_immutable();
-- +goose StatementEnd

-- +goose Down
-- Drops activation state. Use only in a disposable database.
DROP TABLE delivery_receipts;
DROP FUNCTION delivery_receipt_immutable();
DROP TABLE activation_deliveries;
DROP TABLE activations;
DROP TABLE destinations;
