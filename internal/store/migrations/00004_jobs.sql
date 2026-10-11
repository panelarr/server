-- +goose Up
-- Runs of named background jobs (plugin sync, cleanups, update checks),
-- kept per job up to a retention limit.
CREATE TABLE job_runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id       text NOT NULL,
    status       text NOT NULL CHECK (status IN ('running', 'succeeded', 'failed', 'cancelled', 'skipped')),
    trigger      text NOT NULL CHECK (trigger IN ('schedule', 'manual', 'startup', 'system')),
    triggered_by text NOT NULL DEFAULT '',
    params       jsonb NOT NULL DEFAULT '{}',
    message      text NOT NULL DEFAULT '',
    counts       jsonb NOT NULL DEFAULT '{}',
    notes        jsonb NOT NULL DEFAULT '[]',
    progress     jsonb,
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz
);
CREATE INDEX job_runs_job_idx ON job_runs (job_id, started_at DESC, id);

-- The task queue: one row per piece of work (match a work, check a work
-- for updates, download a chapter). A dedupe key keeps one live task per
-- piece of work; a limit key ("source:<id>") lets the queue cap how many
-- run at once per source across all users.
CREATE TABLE tasks (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         text NOT NULL CHECK (kind ~ '^[a-z][a-z0-9_.]*$'),
    payload      jsonb NOT NULL DEFAULT '{}',
    dedupe_key   text NOT NULL DEFAULT '',
    limit_key    text NOT NULL DEFAULT '',
    priority     integer NOT NULL DEFAULT 0,
    state        text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    attempts     integer NOT NULL DEFAULT 0,
    max_attempts integer NOT NULL DEFAULT 5 CHECK (max_attempts > 0),
    run_after    timestamptz NOT NULL DEFAULT now(),
    lease_until  timestamptz,
    last_error   text NOT NULL DEFAULT '',
    progress     jsonb,
    result       jsonb,
    created_by   uuid REFERENCES users (id) ON DELETE SET NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    started_at   timestamptz,
    finished_at  timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX tasks_live_dedupe_key ON tasks (dedupe_key)
    WHERE dedupe_key <> '' AND state IN ('queued', 'running');
CREATE INDEX tasks_claim_idx ON tasks (priority DESC, run_after, created_at) WHERE state = 'queued';
CREATE INDEX tasks_kind_state_idx ON tasks (kind, state, created_at DESC);

-- +goose Down
DROP TABLE tasks;
DROP TABLE job_runs;
