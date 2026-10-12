-- +goose Up
-- Chapters a user bookmarked.
CREATE TABLE bookmarks (
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    chapter_id uuid NOT NULL REFERENCES chapters (id) ON DELETE CASCADE,
    work_id    uuid NOT NULL REFERENCES works (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, chapter_id)
);
CREATE INDEX bookmarks_user_work_idx ON bookmarks (user_id, work_id);

-- +goose Down
DROP TABLE bookmarks;
