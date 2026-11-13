-- name: CreateImport :one
INSERT INTO imports (user_id, format, file_sha256, state, summary)
VALUES (@user_id, @format, @file_sha256, 'previewing', @summary)
RETURNING *;

-- name: GetImport :one
SELECT * FROM imports WHERE id = @id AND user_id = @user_id;

-- name: GetImportByID :one
-- For the import's own tasks, which act for its user.
SELECT * FROM imports WHERE id = $1;

-- name: FindImportByFile :one
-- The user's latest import of the same file that didn't fail or get
-- cancelled: a re-upload offers it instead of starting another.
SELECT * FROM imports
WHERE user_id = @user_id AND file_sha256 = @file_sha256 AND state NOT IN ('failed', 'cancelled')
ORDER BY created_at DESC LIMIT 1;

-- name: RunningImport :one
SELECT * FROM imports WHERE user_id = @user_id AND state IN ('previewing', 'applying') LIMIT 1;

-- name: ListImports :many
SELECT * FROM imports WHERE user_id = @user_id ORDER BY created_at DESC, id DESC LIMIT @lim;

-- name: SetImportTask :exec
UPDATE imports SET task_id = @task_id WHERE id = @id;

-- name: FinishImportPreview :one
-- Ends a preview, unless the import was deleted or moved on meanwhile.
UPDATE imports SET state = @state, summary = summary || @summary::jsonb, message = @message,
    finished_at = CASE WHEN @state::text IN ('failed', 'cancelled') THEN now() ELSE finished_at END
WHERE id = @id AND state = 'previewing'
RETURNING *;

-- name: DeleteImport :one
DELETE FROM imports WHERE id = @id AND user_id = @user_id RETURNING *;

-- name: InsertImportItems :copyfrom
INSERT INTO import_items (import_id, idx, title, author, artist, source_compat_id, source_name, series_key, added_at, categories, chapters, chapters_read, bookmarks)
VALUES (@import_id, @idx, @title, @author, @artist, @source_compat_id, @source_name, @series_key, @added_at, @categories, @chapters, @chapters_read, @bookmarks);

-- name: PendingImportItems :many
-- The next series the preview hasn't matched yet.
SELECT import_id, idx, title, author, artist, source_compat_id, source_name, series_key
FROM import_items WHERE import_id = @import_id AND outcome = 'pending' AND idx > @after
ORDER BY idx LIMIT @lim;

-- name: SetImportItemMatch :exec
UPDATE import_items SET outcome = @outcome, work_id = @work_id, entry_id = @entry_id, source_id = @source_id, source_state = @source_state,
    candidates = @candidates, message = @message
WHERE import_id = @import_id AND idx = @idx;

-- name: ImportOutcomeCounts :many
SELECT outcome, count(*)::int AS series, coalesce(sum(chapters_read), 0)::int AS chapters_read,
    coalesce(sum(bookmarks), 0)::int AS bookmarks
FROM import_items WHERE import_id = $1 GROUP BY outcome ORDER BY outcome;

-- name: ImportMissingSources :many
-- Series per source the preview found missing in Panelarr, for package hints.
SELECT source_compat_id, count(*)::int AS series FROM import_items
WHERE import_id = $1 AND source_state = 'missing'
GROUP BY source_compat_id ORDER BY source_compat_id;

-- name: ListImportItems :many
SELECT import_id, idx, title, author, artist, source_compat_id, source_name, series_key, added_at, categories,
    outcome, work_id, entry_id, source_id, source_state, candidates, chapters_read, bookmarks, pending, message,
    jsonb_array_length(chapters)::int AS chapters
FROM import_items
WHERE import_id = @import_id AND (sqlc.narg(outcome)::text IS NULL OR outcome = sqlc.narg(outcome)) AND idx > @after
ORDER BY idx LIMIT @lim;

-- name: AttachedSeriesByKey :one
-- A work that has the series (by key) attached on one of the sources,
-- preferring the sources in the order given.
SELECT work_id, source_id FROM source_series
WHERE source_id = ANY(@source_ids::text[]) AND key = @key AND state = 'attached'
ORDER BY array_position(@source_ids::text[], source_id) LIMIT 1;

-- name: FailStaleImports :execrows
-- Imports whose task ended without finishing them (an admin cancelled it,
-- or it ran out of attempts) stop counting as running.
UPDATE imports i SET state = 'failed', message = 'the import stopped before it finished', finished_at = now()
WHERE i.user_id = @user_id AND i.state IN ('previewing', 'applying')
  AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = i.task_id AND t.state IN ('queued', 'running'));
