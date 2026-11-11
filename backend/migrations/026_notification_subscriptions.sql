-- +goose Up
-- Notification subscriptions (#64, ADR 0014 amendment): the authorization behind every external
-- change notification. A subscription names a registered destination, the processing purpose the
-- notifications serve (whose active policy version records the lawful basis), whether the recipient is
-- a controller or a processor, the event types and data categories it may receive, and the sources whose
-- records are in scope. It is approved under one policy version of the purpose, and for a non-consent
-- lawful basis it cites the approved assessment behind it; when the purpose's active version changes, it
-- authorizes nothing until a new subscription is approved. Without an active subscription nothing is
-- sent. Scope is immutable: a changed scope is a new subscription, and an old one is suspended.
CREATE TABLE notification_subscriptions (
 id uuid PRIMARY KEY,
 name text NOT NULL UNIQUE CHECK (name ~ '^[a-z][a-z0-9_]{0,63}$'),
 destination text NOT NULL REFERENCES destinations(name),
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 policy_version_id uuid NOT NULL REFERENCES consent_policy_versions(id),
 basis_assessment text CHECK (basis_assessment ~ '^[A-Za-z0-9._:/-]{1,128}$'),
 recipient_role text NOT NULL CHECK (recipient_role IN ('controller','processor')),
 event_types text[] NOT NULL CHECK (cardinality(event_types) BETWEEN 1 AND 2
   AND event_types <@ ARRAY['profile.created.v1','profile.updated.v1']::text[]),
 data_categories text[] NOT NULL CHECK (data_categories = ARRAY['record_reference']::text[]),
 sources text[] NOT NULL CHECK (cardinality(sources) BETWEEN 1 AND 20),
 state text NOT NULL DEFAULT 'active' CHECK (state IN ('active','suspended')),
 created_by text NOT NULL CHECK (char_length(created_by) BETWEEN 1 AND 128),
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- One outcome per event and subscription, written once; it is also the durable de-duplication record.
-- It replaces profile_change_notifications, which held outcomes of the unreleased single-destination
-- development slice (#149) and is dropped with them.
CREATE TABLE notification_deliveries (
 event_id uuid NOT NULL,
 subscription_id uuid NOT NULL REFERENCES notification_subscriptions(id),
 profile_id uuid NOT NULL,
 profile_version bigint NOT NULL CHECK (profile_version > 0),
 outcome text NOT NULL CHECK (outcome IN ('delivered','suppressed','failed')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 status_code integer,
 recorded_at timestamptz NOT NULL DEFAULT now(),
 PRIMARY KEY (event_id, subscription_id)
);
CREATE INDEX notification_deliveries_subscription ON notification_deliveries(subscription_id, recorded_at DESC);
DROP TABLE profile_change_notifications;

-- +goose Down
-- Drops subscriptions and delivery history. Use only in a disposable database.
DROP TABLE notification_deliveries;
DROP TABLE notification_subscriptions;
CREATE TABLE profile_change_notifications (
 event_id uuid PRIMARY KEY,
 profile_id uuid NOT NULL,
 profile_version bigint NOT NULL CHECK (profile_version > 0),
 destination text NOT NULL,
 outcome text NOT NULL CHECK (outcome IN ('delivered','suppressed','failed')),
 reason text NOT NULL CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 status_code integer,
 recorded_at timestamptz NOT NULL DEFAULT now()
);
