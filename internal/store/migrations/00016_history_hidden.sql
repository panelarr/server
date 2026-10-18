-- +goose Up
-- Reading history a user took out of view: one series up to a moment, or
-- everything up to a moment. Read marks (progress) stay as they are.
CREATE TABLE history_hidden (
    user_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    work_id uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    before  timestamptz NOT NULL,
    PRIMARY KEY (user_id, work_id)
);

CREATE TABLE history_cleared (
    user_id uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    before  timestamptz NOT NULL
);

-- +goose Down
DROP TABLE history_cleared;
DROP TABLE history_hidden;
