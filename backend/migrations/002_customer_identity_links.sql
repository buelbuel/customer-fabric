-- +goose Up
CREATE TABLE customer_identity_links (
 kratos_identity_id uuid PRIMARY KEY,
 profile_id uuid NOT NULL UNIQUE REFERENCES profiles(id),
 linked_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- Use only on disposable databases. This drops the identity links.
DROP TABLE customer_identity_links;
