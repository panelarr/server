-- +goose Up
-- Reading progress per user and chapter, synced across devices.
CREATE TABLE progress (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chapter_id uuid NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,
    work_id    uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    page       integer NOT NULL DEFAULT 0 CHECK (page >= 0),
    pages      integer CHECK (pages IS NULL OR pages > 0),
    completed  boolean NOT NULL DEFAULT false,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, chapter_id)
);
CREATE INDEX progress_user_recent_idx ON progress (user_id, updated_at DESC);
CREATE INDEX progress_user_work_idx ON progress (user_id, work_id);

-- Reader settings per user, at three levels: "global", "source:<id>" and
-- "work:<id>"; more specific levels override. The shape belongs to the
-- client.
CREATE TABLE reader_settings (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    scope      text NOT NULL CHECK (scope = 'global' OR scope LIKE 'source:%' OR scope LIKE 'work:%'),
    settings   jsonb NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, scope)
);

-- App preferences per user (theme, colours, fonts); the shape belongs to
-- the client.
CREATE TABLE user_preferences (
    user_id     uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    preferences jsonb NOT NULL,
    updated_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE user_preferences;
DROP TABLE reader_settings;
DROP TABLE progress;
