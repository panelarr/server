-- name: ListStorages :many
SELECT * FROM storages ORDER BY is_default DESC, id;

-- name: GetStorage :one
SELECT * FROM storages WHERE id = $1;

-- name: GetDefaultStorage :one
SELECT * FROM storages WHERE is_default;

-- name: CreateStorage :one
INSERT INTO storages (id, name, kind, config, secret, is_default)
VALUES (@id, @name, @kind, @config, @secret, @is_default)
RETURNING *;

-- name: UpdateStorage :one
UPDATE storages SET name = @name, config = @config, secret = @secret, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ClearDefaultStorage :exec
UPDATE storages SET is_default = false WHERE is_default;

-- name: SetDefaultStorage :execrows
UPDATE storages SET is_default = true, updated_at = now() WHERE id = $1;

-- name: DeleteStorage :execrows
DELETE FROM storages WHERE id = $1;

-- name: CountFilesByStorage :one
SELECT count(*) FROM chapter_files WHERE storage_id = $1;

-- name: GetChapterFile :one
SELECT * FROM chapter_files WHERE chapter_id = $1;

-- name: InsertChapterFile :one
INSERT INTO chapter_files (chapter_id, storage_id, key, pages, bytes, sha256, from_variant_id)
VALUES (@chapter_id, @storage_id, @key, @pages, @bytes, @sha256, @from_variant_id)
RETURNING *;

-- name: MoveChapterFile :exec
UPDATE chapter_files SET storage_id = @storage_id, key = @key WHERE id = @id;

-- name: DeleteChapterFile :execrows
DELETE FROM chapter_files WHERE id = $1;

-- name: FilesOnStorage :many
SELECT * FROM chapter_files WHERE storage_id = @storage_id ORDER BY created_at, id LIMIT @lim;

-- name: DownloadedChapterIDs :many
SELECT chapter_files.chapter_id FROM chapter_files
JOIN chapters ON chapters.id = chapter_files.chapter_id
WHERE chapters.work_id = $1;

-- name: SetAutoDownload :execrows
UPDATE library_entries SET auto_download = @auto_download WHERE user_id = @user_id AND work_id = @work_id;

-- name: AutoDownloadUsers :many
SELECT library_entries.user_id FROM library_entries
WHERE library_entries.work_id = $1 AND library_entries.auto_download;

-- name: ListDownloadTasks :many
-- Download tasks with their chapter and work, newest first; users see the
-- ones they asked for, admins all.
SELECT sqlc.embed(tasks), chapters.id AS chapter_id, chapters.number AS chapter_number, chapters.title AS chapter_title,
       works.id AS work_id, works.title AS work_title
FROM tasks
JOIN chapters ON chapters.id = (tasks.payload->>'chapter_id')::uuid
JOIN works ON works.id = chapters.work_id
WHERE tasks.kind = 'download.chapter'
  AND (sqlc.narg(user_id)::uuid IS NULL OR tasks.created_by = sqlc.narg(user_id)::uuid)
  AND (sqlc.narg(state)::text IS NULL OR tasks.state = sqlc.narg(state))
  AND (sqlc.narg(before)::timestamptz IS NULL OR (tasks.created_at, tasks.id) < (sqlc.narg(before)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY tasks.created_at DESC, tasks.id DESC
LIMIT @lim;

-- name: ChapterFileByKey :one
SELECT * FROM chapter_files WHERE storage_id = @storage_id AND key = @key;
