-- +goose Up
-- When a chapter first turned up in a later check of a series it wasn't in
-- before: what the updates feed lists. Chapters a series had when it was
-- first listed, and chapters that come from rebuilding the unified list,
-- never appeared this way and stay NULL.
ALTER TABLE chapters ADD COLUMN appeared_at timestamptz;
CREATE INDEX chapters_appeared_idx ON chapters (work_id, appeared_at DESC) WHERE appeared_at IS NOT NULL;

-- A variant a later check found: the next rebuild of the unified list
-- announces the chapter it makes for it (if it makes one), then clears it.
ALTER TABLE source_chapters ADD COLUMN announce boolean NOT NULL DEFAULT false;

-- When a series was last listed without an error: later listings announce.
ALTER TABLE source_series ADD COLUMN listed_at timestamptz;
UPDATE source_series SET listed_at = checked_at
WHERE checked_at IS NOT NULL AND EXISTS (SELECT 1 FROM source_chapters WHERE source_chapters.source_series_id = source_series.id);

-- Before this, a work's first chapters were all inserted at once; the ones
-- after them are the best guess at what appeared later.
UPDATE chapters SET appeared_at = created_at
WHERE created_at > (SELECT min(c.created_at) FROM chapters AS c WHERE c.work_id = chapters.work_id);

-- +goose Down
ALTER TABLE source_series DROP COLUMN listed_at;
ALTER TABLE source_chapters DROP COLUMN announce;
DROP INDEX chapters_appeared_idx;
ALTER TABLE chapters DROP COLUMN appeared_at;
