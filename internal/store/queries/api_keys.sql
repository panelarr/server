-- name: CreateAPIKey :one
INSERT INTO api_keys (user_id, name, prefix, key_hash, scopes, expires_at)
VALUES (@user_id, @name, @prefix, @key_hash, @scopes, @expires_at)
RETURNING *;

-- name: GetAPIKeyByHash :one
SELECT sqlc.embed(api_keys), sqlc.embed(users)
FROM api_keys JOIN users ON users.id = api_keys.user_id
WHERE api_keys.key_hash = $1;

-- name: TouchAPIKey :exec
UPDATE api_keys SET last_used_at = now() WHERE id = $1;

-- name: ListAPIKeysByUser :many
SELECT * FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC, id;

-- name: DeleteUserAPIKey :execrows
DELETE FROM api_keys WHERE id = @id AND user_id = @user_id;
