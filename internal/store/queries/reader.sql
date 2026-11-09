-- name: UpsertProgress :one
-- A chapter once read stays read when it is opened again: only an explicit
-- completed (set_completed) can clear it.
INSERT INTO progress (user_id, chapter_id, work_id, page, pages, completed)
VALUES (@user_id, @chapter_id, @work_id, @page, @pages, @completed)
ON CONFLICT (user_id, chapter_id) DO UPDATE SET
    page = EXCLUDED.page, pages = COALESCE(EXCLUDED.pages, progress.pages),
    completed = CASE WHEN @set_completed::boolean THEN EXCLUDED.completed
                     ELSE progress.completed OR EXCLUDED.completed END,
    updated_at = now()
RETURNING *;

-- name: GetProgress :one
SELECT * FROM progress WHERE user_id = @user_id AND chapter_id = @chapter_id;

-- name: WorkProgress :many
SELECT * FROM progress WHERE user_id = @user_id AND work_id = @work_id;

-- name: DeleteProgress :exec
DELETE FROM progress WHERE user_id = @user_id AND chapter_id = ANY(@chapter_ids::uuid[]);

-- name: History :many
-- A user's reading, newest first, without what they took out of view;
-- one entry per chapter, or per series (the latest chapter) with
-- per_series; narrowed to a series or by its title.
WITH visible AS (
    SELECT progress.* FROM progress
    LEFT JOIN history_hidden ON history_hidden.user_id = progress.user_id AND history_hidden.work_id = progress.work_id
    LEFT JOIN history_cleared ON history_cleared.user_id = progress.user_id
    WHERE progress.user_id = @user_id
      AND (history_hidden.before IS NULL OR progress.updated_at > history_hidden.before)
      AND (history_cleared.before IS NULL OR progress.updated_at > history_cleared.before)
      AND (sqlc.narg(work_id)::uuid IS NULL OR progress.work_id = sqlc.narg(work_id)::uuid)
), entries AS (
    SELECT * FROM visible WHERE NOT @per_series::boolean
    UNION ALL
    (SELECT DISTINCT ON (work_id) * FROM visible WHERE @per_series::boolean
     ORDER BY work_id, updated_at DESC, chapter_id DESC)
)
SELECT sqlc.embed(progress), sqlc.embed(chapters), works.title AS work_title, works.cover_url AS work_cover_url
FROM entries AS progress
JOIN chapters ON chapters.id = progress.chapter_id
JOIN works ON works.id = progress.work_id
WHERE (sqlc.narg(q)::text IS NULL OR works.title ILIKE '%' || sqlc.narg(q)::text || '%')
  AND (sqlc.narg(before)::timestamptz IS NULL
       OR (progress.updated_at, progress.chapter_id) < (sqlc.narg(before)::timestamptz, sqlc.narg(before_chapter)::uuid))
ORDER BY progress.updated_at DESC, progress.chapter_id DESC
LIMIT @lim;

-- name: HideHistory :exec
INSERT INTO history_hidden (user_id, work_id, before) VALUES (@user_id, @work_id, now())
ON CONFLICT (user_id, work_id) DO UPDATE SET before = now();

-- name: ClearHistory :exec
INSERT INTO history_cleared (user_id, before) VALUES (@user_id, now())
ON CONFLICT (user_id) DO UPDATE SET before = now();

-- name: ListReaderSettings :many
SELECT * FROM reader_settings WHERE user_id = @user_id AND scope = ANY(@scopes::text[]);

-- name: PutReaderSettings :one
INSERT INTO reader_settings (user_id, scope, settings) VALUES (@user_id, @scope, @settings)
ON CONFLICT (user_id, scope) DO UPDATE SET settings = EXCLUDED.settings, updated_at = now()
RETURNING *;

-- name: DeleteReaderSettings :execrows
DELETE FROM reader_settings WHERE user_id = @user_id AND scope = @scope;

-- name: GetPreferences :one
SELECT * FROM user_preferences WHERE user_id = $1;

-- name: PutPreferences :one
INSERT INTO user_preferences (user_id, preferences) VALUES (@user_id, @preferences)
ON CONFLICT (user_id) DO UPDATE SET preferences = EXCLUDED.preferences, updated_at = now()
RETURNING *;

-- name: AddBookmarks :exec
INSERT INTO bookmarks (user_id, chapter_id, work_id)
SELECT @user_id, unnest(@chapter_ids::uuid[]), @work_id
ON CONFLICT (user_id, chapter_id) DO NOTHING;

-- name: DeleteBookmarks :exec
DELETE FROM bookmarks WHERE user_id = @user_id AND chapter_id = ANY(@chapter_ids::uuid[]);

-- name: ListBookmarks :many
SELECT chapter_id FROM bookmarks WHERE user_id = @user_id AND work_id = @work_id ORDER BY created_at, chapter_id;

-- name: CountBookmarks :many
SELECT work_id, count(*) AS bookmarks FROM bookmarks
WHERE user_id = @user_id AND work_id = ANY(@work_ids::uuid[])
GROUP BY work_id;

-- name: LibraryChapters :many
-- The chapters of some works, with when their chosen copy was uploaded
-- and when a later check found them.
SELECT chapters.id, chapters.work_id, chapters.number, chapters.available, chapters.created_at,
       chapters.appeared_at, source_chapters.uploaded_at
FROM chapters LEFT JOIN source_chapters ON source_chapters.id = chapters.preferred_variant_id
WHERE chapters.work_id = ANY(@work_ids::uuid[]);

-- name: LibraryProgress :many
SELECT * FROM progress WHERE user_id = @user_id AND work_id = ANY(@work_ids::uuid[]);

-- name: CountDownloadedByWork :many
SELECT chapters.work_id, count(*) AS downloaded
FROM chapter_files JOIN chapters ON chapters.id = chapter_files.chapter_id
WHERE chapters.work_id = ANY(@work_ids::uuid[])
GROUP BY chapters.work_id;
