-- name: UpsertProgress :one
INSERT INTO progress (user_id, chapter_id, work_id, page, pages, completed)
VALUES (@user_id, @chapter_id, @work_id, @page, @pages, @completed)
ON CONFLICT (user_id, chapter_id) DO UPDATE SET
    page = EXCLUDED.page, pages = COALESCE(EXCLUDED.pages, progress.pages),
    completed = EXCLUDED.completed, updated_at = now()
RETURNING *;

-- name: GetProgress :one
SELECT * FROM progress WHERE user_id = @user_id AND chapter_id = @chapter_id;

-- name: WorkProgress :many
SELECT * FROM progress WHERE user_id = @user_id AND work_id = @work_id;

-- name: DeleteProgress :exec
DELETE FROM progress WHERE user_id = @user_id AND chapter_id = ANY(@chapter_ids::uuid[]);

-- name: History :many
SELECT sqlc.embed(progress), sqlc.embed(chapters), works.title AS work_title, works.cover_url AS work_cover_url
FROM progress
JOIN chapters ON chapters.id = progress.chapter_id
JOIN works ON works.id = progress.work_id
WHERE progress.user_id = @user_id
  AND (sqlc.narg(before)::timestamptz IS NULL
       OR (progress.updated_at, progress.chapter_id) < (sqlc.narg(before)::timestamptz, sqlc.narg(before_chapter)::uuid))
ORDER BY progress.updated_at DESC, progress.chapter_id DESC
LIMIT @lim;

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
