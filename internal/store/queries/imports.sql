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
-- Newest first, after a (created_at, id) cursor.
SELECT * FROM imports WHERE user_id = @user_id
  AND (sqlc.narg(before_created)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(before_created)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC LIMIT @lim;

-- name: CountImports :one
SELECT count(*)::int FROM imports WHERE user_id = $1;

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

-- name: ImportResultCounts :many
-- Series per result of apply (those apply went through or skipped).
SELECT result, count(*)::int AS series FROM import_items
WHERE import_id = $1 AND result <> '' GROUP BY result ORDER BY result;

-- name: ImportMissingSources :many
-- Series per source the preview found missing in Panelarr, for package hints.
SELECT source_compat_id, count(*)::int AS series FROM import_items
WHERE import_id = $1 AND source_state = 'missing'
GROUP BY source_compat_id ORDER BY source_compat_id;

-- name: ListImportItems :many
SELECT import_id, idx, title, author, artist, source_compat_id, source_name, series_key, added_at, categories,
    outcome, work_id, entry_id, source_id, source_state, candidates, chapters_read, bookmarks, pending, message,
    result, applied_at, placed, unplaced, jsonb_array_length(chapters)::int AS chapters
FROM import_items
WHERE import_id = @import_id AND (sqlc.narg(outcome)::text IS NULL OR outcome = sqlc.narg(outcome))
  AND (sqlc.narg(result)::text IS NULL OR result = sqlc.narg(result)) AND idx > @after
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

-- name: StartImportApply :one
-- From preview, or again after a cancel or a failure (it resumes): once
-- every series was matched.
UPDATE imports SET state = 'applying', message = '', finished_at = NULL
WHERE id = @id AND user_id = @user_id AND state IN ('preview', 'cancelled', 'failed')
  AND NOT EXISTS (SELECT 1 FROM import_items WHERE import_id = imports.id AND outcome = 'pending')
RETURNING *;

-- name: AddImportClampedTimes :exec
-- Counts read times from the future that apply capped, for the report.
UPDATE imports SET summary = jsonb_set(summary, '{clamped_times}',
    to_jsonb(coalesce((summary->>'clamped_times')::int, 0) + @clamped::int))
WHERE id = @id;

-- name: FinishImportApply :one
-- Ends an apply, unless the import was cancelled or deleted meanwhile.
UPDATE imports SET state = @state, message = @message, finished_at = now()
WHERE id = @id AND state = 'applying'
RETURNING *;

-- name: CancelImport :one
UPDATE imports SET state = 'cancelled', message = 'cancelled', finished_at = now()
WHERE id = @id AND user_id = @user_id AND state IN ('previewing', 'applying')
RETURNING *;

-- name: ImportItemsToApply :many
-- The next series apply hasn't gone through yet, in file order.
SELECT * FROM import_items
WHERE import_id = @import_id AND applied_at IS NULL AND result = '' AND outcome IN ('ready', 'in_library', 'source_missing') AND idx > @after
ORDER BY idx LIMIT @lim;

-- name: ImportApplyProgress :one
SELECT count(*) FILTER (WHERE applied_at IS NOT NULL)::int AS done,
    count(*) FILTER (WHERE applied_at IS NOT NULL OR (result = '' AND outcome IN ('ready', 'in_library', 'source_missing')))::int AS total
FROM import_items WHERE import_id = $1;

-- name: GetImportItem :one
SELECT * FROM import_items WHERE import_id = @import_id AND idx = @idx;

-- name: LockImportItem :one
-- An import's series with whose it is, locked while its chapters are placed.
SELECT sqlc.embed(import_items), imports.user_id, imports.created_at AS import_created_at
FROM import_items JOIN imports ON imports.id = import_items.import_id
WHERE import_items.import_id = @import_id AND import_items.idx = @idx
FOR UPDATE OF import_items;

-- name: FinishImportItem :exec
UPDATE import_items SET outcome = @outcome, result = @result, work_id = @work_id, entry_id = @entry_id, source_id = @source_id,
    message = @message, placed = @placed, unplaced = @unplaced, pending = @pending,
    pending_chapters = @pending_chapters, pending_until = @pending_until, applied_at = now()
WHERE import_id = @import_id AND idx = @idx;

-- name: SetImportItemPending :exec
UPDATE import_items SET placed = @placed, unplaced = @unplaced, pending = @pending,
    pending_chapters = @pending_chapters, pending_until = @pending_until, message = @message
WHERE import_id = @import_id AND idx = @idx;

-- name: SetImportItemChoice :execrows
-- A series resolved by hand, or skipped: not once it was applied, nor
-- while its import runs.
UPDATE import_items SET outcome = @outcome, result = @result, work_id = @work_id, entry_id = @entry_id, source_id = @source_id,
    source_state = @source_state, message = @message
WHERE import_id = @import_id AND idx = @idx AND result NOT IN ('applied', 'already_imported')
  AND EXISTS (SELECT 1 FROM imports WHERE imports.id = import_items.import_id AND imports.state NOT IN ('previewing', 'applying'));

-- name: WorkHasPendingImports :one
SELECT EXISTS (SELECT 1 FROM import_items WHERE work_id = $1 AND pending > 0);

-- name: PendingImportItemsOnWork :many
SELECT import_id, idx FROM import_items WHERE work_id = $1 AND pending > 0 ORDER BY import_id, idx;

-- name: PendingImportItemsOfImport :many
SELECT idx, pending_until FROM import_items WHERE import_id = $1 AND pending > 0 ORDER BY idx;

-- name: ChaptersBySourceKey :many
-- A work's chapters by the keys of one attached series' chapters.
SELECT sc.key, sc.chapter_id::uuid AS chapter_id
FROM source_chapters sc JOIN source_series ss ON ss.id = sc.source_series_id
WHERE ss.work_id = @work_id AND ss.source_id = @source_id AND ss.key = @series_key AND ss.state = 'attached'
  AND sc.key = ANY(@keys::text[]) AND sc.chapter_id IS NOT NULL;

-- name: ChapterNumbers :many
SELECT id, number::float8 AS number FROM chapters WHERE work_id = $1 AND number IS NOT NULL;

-- name: ImportProgress :execrows
-- Imported reading never loses newer reading: a read chapter stays read,
-- a page or time newer in Panelarr is kept, and the original times are
-- kept (History shows them). A row it doesn't change isn't written.
INSERT INTO progress (user_id, chapter_id, work_id, page, completed, updated_at)
SELECT @user_id, c.id, c.work_id, x.page, x.completed, x.read_at
FROM (SELECT unnest(@chapter_ids::uuid[]) AS chapter_id, unnest(@pages::int[]) AS page,
             unnest(@completed::boolean[]) AS completed, unnest(@read_at::timestamptz[]) AS read_at) AS x
JOIN chapters c ON c.id = x.chapter_id
ON CONFLICT (user_id, chapter_id) DO UPDATE SET
    completed  = progress.completed OR EXCLUDED.completed,
    page       = CASE WHEN EXCLUDED.updated_at > progress.updated_at AND NOT progress.completed
                      THEN EXCLUDED.page ELSE progress.page END,
    updated_at = GREATEST(progress.updated_at, EXCLUDED.updated_at)
WHERE (EXCLUDED.completed AND NOT progress.completed) OR EXCLUDED.updated_at > progress.updated_at;

-- name: ImportBookmarks :execrows
-- Bookmarks an import adds; existing ones (and their times) stay.
INSERT INTO bookmarks (user_id, chapter_id, work_id, created_at)
SELECT @user_id, c.id, c.work_id, x.created_at
FROM (SELECT unnest(@chapter_ids::uuid[]) AS chapter_id, unnest(@created_at::timestamptz[]) AS created_at) AS x
JOIN chapters c ON c.id = x.chapter_id
ON CONFLICT (user_id, chapter_id) DO NOTHING;
