-- name: UpsertSourceChapter :exec
INSERT INTO source_chapters (source_series_id, key, name, number, volume, scanlator, lang, url, uploaded_at, position)
VALUES (@source_series_id, @key, @name, @number, @volume, @scanlator, @lang, @url, @uploaded_at, @position)
ON CONFLICT (source_series_id, key) DO UPDATE SET
    name = EXCLUDED.name, number = EXCLUDED.number, volume = EXCLUDED.volume, scanlator = EXCLUDED.scanlator,
    lang = EXCLUDED.lang, url = EXCLUDED.url, uploaded_at = EXCLUDED.uploaded_at, position = EXCLUDED.position,
    present = true, last_seen_at = now();

-- name: MarkSourceChaptersAbsent :exec
UPDATE source_chapters SET present = false
WHERE source_series_id = @source_series_id AND present AND NOT (key = ANY(@listed::text[]));

-- name: SetSeriesChecked :exec
UPDATE source_series SET checked_at = now(), check_error = @check_error WHERE id = @id;

-- name: ListSourceChaptersBySeries :many
SELECT * FROM source_chapters WHERE source_series_id = $1 ORDER BY position, id;

-- name: LockWork :exec
SELECT 1 FROM works WHERE id = $1 FOR UPDATE;

-- name: WorkVariants :many
-- Present chapters of a work's attached series, with what decides the
-- preferred variant.
SELECT sqlc.embed(source_chapters), source_series.source_id, source_series.priority AS series_priority,
       sources.health AS source_health, sources.name AS source_name, sources.lang AS source_lang,
       (sources.present AND sources.enabled AND plugins.enabled)::boolean AS source_usable
FROM source_chapters
JOIN source_series ON source_series.id = source_chapters.source_series_id
JOIN sources ON sources.id = source_series.source_id
JOIN plugins ON plugins.id = sources.plugin_id
WHERE source_series.work_id = $1 AND source_series.state = 'attached' AND source_chapters.present;

-- name: WorksOnSources :many
-- Works with a series attached on any of these sources.
SELECT DISTINCT work_id FROM source_series
WHERE state = 'attached' AND source_id = ANY(@source_ids::text[]);

-- name: ListChapters :many
SELECT * FROM chapters WHERE work_id = $1;

-- name: GetChapter :one
SELECT * FROM chapters WHERE id = $1;

-- name: InsertChapter :one
INSERT INTO chapters (work_id, number, extra_key, volume, title, preferred_variant_id, reason, available)
VALUES (@work_id, @number, @extra_key, @volume, @title, @preferred_variant_id, @reason, true)
RETURNING *;

-- name: UpdateChapter :exec
UPDATE chapters SET volume = @volume, title = @title, preferred_variant_id = @preferred_variant_id,
    reason = @reason, available = @available, updated_at = now()
WHERE id = @id;

-- name: SetChapterPin :exec
UPDATE chapters SET pinned_variant_id = @pinned_variant_id, updated_at = now() WHERE id = @id;

-- name: AssignVariants :exec
UPDATE source_chapters SET chapter_id = @chapter_id WHERE id = ANY(@ids::uuid[]);

-- name: ChapterVariants :many
SELECT sqlc.embed(source_chapters), source_series.source_id, source_series.priority AS series_priority,
       sources.health AS source_health, sources.name AS source_name, sources.lang AS source_lang,
       (sources.present AND sources.enabled AND plugins.enabled)::boolean AS source_usable
FROM source_chapters
JOIN source_series ON source_series.id = source_chapters.source_series_id
JOIN sources ON sources.id = source_series.source_id
JOIN plugins ON plugins.id = sources.plugin_id
WHERE source_chapters.chapter_id = @chapter_id::uuid AND source_series.state = 'attached' AND source_chapters.present
ORDER BY source_series.priority DESC, source_series.source_id;

-- name: CountChaptersByWork :many
SELECT work_id, count(*) FILTER (WHERE available) AS total,
       count(*) FILTER (WHERE available AND created_at > @since::timestamptz) AS recent
FROM chapters WHERE work_id = ANY(@work_ids::uuid[]) GROUP BY work_id;

-- name: NewChaptersForUser :many
-- Chapters found after each library entry was last seen.
SELECT library_entries.work_id, count(chapters.id) AS new
FROM library_entries JOIN chapters ON chapters.work_id = library_entries.work_id
WHERE library_entries.user_id = @user_id AND library_entries.work_id = ANY(@work_ids::uuid[])
  AND chapters.available AND chapters.created_at > library_entries.seen_at
GROUP BY library_entries.work_id;

-- name: MarkLibrarySeen :execrows
UPDATE library_entries SET seen_at = now() WHERE user_id = @user_id AND work_id = @work_id;

-- name: AttachedSeriesOfFollowedWorks :many
-- Attached series of works someone follows: what scheduled update checks
-- look at.
SELECT DISTINCT source_series.* FROM source_series
JOIN library_entries ON library_entries.work_id = source_series.work_id
WHERE source_series.state = 'attached';

-- name: WorksWithAttachedSeries :many
SELECT DISTINCT work_id FROM source_series WHERE state = 'attached' ORDER BY work_id;
