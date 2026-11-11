-- +goose Up
-- Durable audience jobs (#143). A job evaluates one pinned segment revision over the whole customer
-- population outside any request. Each attempt decides every customer in one REPEATABLE READ
-- transaction, so membership reflects one database state, and publishes its snapshot or activation and
-- completes the job in that same transaction. An attempt that fails, is cancelled or is interrupted
-- rolls back and publishes nothing; a retry evaluates again under a new snapshot.
CREATE TABLE audience_jobs (
 id uuid PRIMARY KEY,
 kind text NOT NULL CHECK (kind IN ('snapshot','activation')),
 segment_name text NOT NULL,
 segment_revision integer NOT NULL,
 purpose_id uuid NOT NULL REFERENCES consent_purposes(id),
 destination text REFERENCES destinations(name),
 idempotency_key text NOT NULL UNIQUE CHECK (idempotency_key ~ '^[A-Za-z0-9_-]{16,128}$'),
 state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','running','completed','failed','cancelled')),
 -- reason is a fixed code, never customer data.
 reason text CHECK (reason ~ '^[a-z_0-9]{1,64}$'),
 -- Evaluation attempts started, including any cut short by a restart.
 attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
 -- Final counts and the evaluation time of the attempt that completed; zero and NULL until then.
 evaluated_at timestamptz,
 population integer NOT NULL DEFAULT 0 CHECK (population >= 0),
 members integer NOT NULL DEFAULT 0 CHECK (members >= 0),
 excluded_unavailable integer NOT NULL DEFAULT 0 CHECK (excluded_unavailable >= 0),
 excluded_consent integer NOT NULL DEFAULT 0 CHECK (excluded_consent >= 0),
 excluded_criteria integer NOT NULL DEFAULT 0 CHECK (excluded_criteria >= 0),
 -- The snapshot or activation a completed job published.
 result_id uuid,
 created_at timestamptz NOT NULL DEFAULT now(),
 updated_at timestamptz NOT NULL DEFAULT now(),
 finished_at timestamptz,
 CHECK ((kind = 'activation') = (destination IS NOT NULL)),
 CHECK ((state = 'completed') = (result_id IS NOT NULL)),
 CHECK ((state = 'completed') = (evaluated_at IS NOT NULL)),
 CHECK ((state IN ('completed','failed','cancelled')) = (finished_at IS NOT NULL)),
 FOREIGN KEY (segment_name, segment_revision) REFERENCES segment_definitions(name, revision)
);
CREATE INDEX audience_jobs_pending ON audience_jobs(updated_at, id) WHERE state IN ('queued','running');
CREATE INDEX audience_jobs_recent ON audience_jobs(created_at DESC, id);

-- How many customers the current attempt has decided. It is kept apart from the job so that reporting
-- progress never changes the row the evaluating transaction completes; it is a count, nothing else.
CREATE TABLE audience_job_progress (
 job_id uuid PRIMARY KEY REFERENCES audience_jobs(id),
 attempt integer NOT NULL,
 evaluated integer NOT NULL CHECK (evaluated >= 0),
 updated_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- Drops job history. Use only in a disposable database.
DROP TABLE audience_job_progress;
DROP TABLE audience_jobs;
