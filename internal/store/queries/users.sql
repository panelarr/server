-- name: CountUsers :one
SELECT count(*) FROM users;

-- name: CountEnabledAdmins :one
SELECT count(*) FROM users WHERE role = 'admin' AND NOT disabled;

-- name: LockAccounts :exec
-- Serialises changes that check "is there another admin?" or "is this the
-- first account?" within one transaction.
SELECT pg_advisory_xact_lock(hashtext('panelarr.accounts'));

-- name: CreateUser :one
INSERT INTO users (
    username, display_name, password_hash, role,
    may_add, may_download, may_manage_matches, content_rating_ceiling, all_sources, allowed_sources
) VALUES (
    @username, @display_name, @password_hash, @role,
    @may_add, @may_download, @may_manage_matches, @content_rating_ceiling, @all_sources, @allowed_sources
)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserForUpdate :one
SELECT * FROM users WHERE id = $1 FOR UPDATE;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE lower(username) = lower(@username);

-- name: ListUsers :many
SELECT * FROM users
WHERE lower(username) > @after
ORDER BY lower(username)
LIMIT @lim;

-- name: UpdateUser :one
UPDATE users SET
    display_name = @display_name,
    role = @role,
    disabled = @disabled,
    may_add = @may_add,
    may_download = @may_download,
    may_manage_matches = @may_manage_matches,
    content_rating_ceiling = @content_rating_ceiling,
    all_sources = @all_sources,
    allowed_sources = @allowed_sources,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: SetUserPassword :exec
UPDATE users SET password_hash = @password_hash, password_changed_at = now(), updated_at = now()
WHERE id = @id;

-- name: TouchUserLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: DeleteUser :execrows
DELETE FROM users WHERE id = $1;

-- name: SetUserRole :one
UPDATE users SET role = @role, updated_at = now() WHERE id = @id RETURNING *;
