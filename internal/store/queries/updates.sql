-- name: Updates :many
-- Chapters that appeared in these works of a user's library since they
-- added each, newest first, with the user's progress and the chapter's
-- download.
SELECT sqlc.embed(chapters), works.title AS work_title, works.cover_url AS work_cover_url,
       (chapters.appeared_at > library_entries.seen_at)::boolean AS new,
       progress.page AS progress_page, progress.pages AS progress_pages,
       coalesce(progress.completed, false)::boolean AS completed,
       (chapter_files.id IS NOT NULL)::boolean AS downloaded,
       coalesce(downloads.state, '')::text AS download_state
FROM chapters
JOIN library_entries ON library_entries.work_id = chapters.work_id AND library_entries.user_id = @user_id
JOIN works ON works.id = chapters.work_id
LEFT JOIN progress ON progress.user_id = @user_id AND progress.chapter_id = chapters.id
LEFT JOIN chapter_files ON chapter_files.chapter_id = chapters.id
LEFT JOIN tasks AS downloads ON downloads.dedupe_key = 'download:' || chapters.id::text AND downloads.state IN ('queued', 'running')
WHERE chapters.work_id = ANY(@work_ids::uuid[]) AND chapters.available
  AND chapters.appeared_at > library_entries.added_at
  AND (sqlc.narg(before)::timestamptz IS NULL
       OR (chapters.appeared_at, chapters.id) < (sqlc.narg(before)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY chapters.appeared_at DESC, chapters.id DESC
LIMIT @lim;

-- name: UpdateChecksOf :one
-- How the chapter checks of these works' attached series stand.
SELECT max(source_series.checked_at) AS last_checked_at,
       count(*) FILTER (WHERE source_series.check_error <> '') AS failing,
       (SELECT count(*) FROM tasks JOIN source_series AS s ON s.id = (tasks.payload ->> 'series_id')::uuid
        WHERE tasks.kind = 'update.series' AND s.work_id = ANY(@work_ids::uuid[])
          AND (tasks.state = 'running' OR (tasks.state = 'queued' AND tasks.run_after <= now()))) AS checking
FROM source_series
WHERE source_series.work_id = ANY(@work_ids::uuid[]) AND source_series.state = 'attached';
