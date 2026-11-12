-- +goose Up
-- Webhook signing secrets at rest (#64). A destination's HMAC secret is sealed with AES-256-GCM under a
-- deployment key named by key_id. Rotating a destination's secret adds a version: the newest signs every
-- send, and the one before it also signs until retires_at, so a recipient can switch over without
-- rejecting deliveries. A secret past retires_at is erased. The operator audit records who created or
-- rotated a secret.
--
-- Existing secrets are not touched here: SQL cannot seal them. The application seals each one as the same
-- value (version 1), checks that it opens to that value, and keeps the plaintext until an operator
-- finalizes the migration, which checks every destination again before clearing it. Until a destination
-- has a sealed secret, its plaintext signs; afterwards only sealed secrets do.

-- Each deployment key seals a fixed check value when first used. A key configured under a known ID that
-- does not open its check value is the wrong key: nothing is sealed with it.
CREATE TABLE sealing_keys (
 id text PRIMARY KEY CHECK (id ~ '^[a-z0-9][a-z0-9_-]{0,31}$'),
 check_value bytea NOT NULL CHECK (octet_length(check_value) = 60),
 created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE destination_secrets (
 destination text NOT NULL REFERENCES destinations(name),
 version integer NOT NULL CHECK (version > 0),
 algorithm text NOT NULL DEFAULT 'aes-256-gcm' CHECK (algorithm = 'aes-256-gcm'),
 key_id text REFERENCES sealing_keys(id),
 -- 12-byte nonce, 32-byte secret, 16-byte tag.
 sealed bytea CHECK (octet_length(sealed) = 60),
 created_at timestamptz NOT NULL DEFAULT now(),
 retires_at timestamptz,
 erased_at timestamptz,
 PRIMARY KEY (destination, version),
 CHECK ((sealed IS NULL) = (key_id IS NULL) AND (sealed IS NULL) = (erased_at IS NOT NULL)),
 CHECK (erased_at IS NULL OR retires_at IS NOT NULL)
);
CREATE INDEX destination_secrets_retiring ON destination_secrets(retires_at) WHERE sealed IS NOT NULL AND retires_at IS NOT NULL;
CREATE INDEX destination_secrets_key ON destination_secrets(key_id) WHERE sealed IS NOT NULL;

ALTER TABLE destinations ALTER COLUMN signing_secret DROP NOT NULL;

-- +goose Down
-- Refuses while any destination lacks its plaintext secret: run `api secrets rollback` first, which
-- restores each destination's newest secret. Dropping sealed secrets is safe only after that.
-- +goose StatementBegin
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM destinations WHERE signing_secret IS NULL) THEN
    RAISE EXCEPTION 'restore plaintext signing secrets with "api secrets rollback" before rolling back';
  END IF;
END $$;
-- +goose StatementEnd
DROP TABLE destination_secrets;
DROP TABLE sealing_keys;
ALTER TABLE destinations ALTER COLUMN signing_secret SET NOT NULL;
