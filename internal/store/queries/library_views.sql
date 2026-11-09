-- name: LockLibraryViews :exec
-- Serialises changes to one user's views, so the limit, the single
-- default and the order hold under concurrent requests.
SELECT pg_advisory_xact_lock(hashtextextended('panelarr.library_views:' || CAST(sqlc.arg(user_id)::uuid AS text), 0));

-- name: ListLibraryViews :many
SELECT * FROM library_views WHERE user_id = @user_id ORDER BY position, created_at, id;

-- name: GetLibraryView :one
SELECT * FROM library_views WHERE id = @id AND user_id = @user_id;

-- name: CountLibraryViews :one
SELECT count(*) FROM library_views WHERE user_id = @user_id;

-- name: CreateLibraryView :one
INSERT INTO library_views (user_id, name, position, is_default, definition)
VALUES (@user_id, @name, @position, @is_default, @definition)
RETURNING *;

-- name: UpdateLibraryView :one
UPDATE library_views SET
    name = coalesce(sqlc.narg(name), name),
    definition = coalesce(sqlc.narg(definition), definition),
    updated_at = now()
WHERE id = @id AND user_id = @user_id
RETURNING *;

-- name: ClearLibraryViewDefault :exec
UPDATE library_views SET is_default = false, updated_at = now()
WHERE user_id = @user_id AND is_default AND id <> @keep;

-- name: SetLibraryViewDefault :exec
UPDATE library_views SET is_default = true, updated_at = now()
WHERE id = @id AND user_id = @user_id AND NOT is_default;

-- name: DeleteLibraryView :one
DELETE FROM library_views WHERE id = @id AND user_id = @user_id RETURNING *;

-- name: ShiftLibraryViewsUp :exec
-- Closes the gap a deleted view left in the order.
UPDATE library_views SET position = position - 1 WHERE user_id = @user_id AND position > @position;

-- name: OrderLibraryViews :exec
UPDATE library_views SET position = o.ord - 1
FROM unnest(@ids::uuid[]) WITH ORDINALITY AS o (id, ord)
WHERE library_views.id = o.id AND library_views.user_id = @user_id;
