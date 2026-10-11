-- name: GetSetting :one
SELECT value FROM settings WHERE key = $1;

-- name: ListSettings :many
SELECT key, value, updated_at FROM settings ORDER BY key;

-- name: PutSetting :exec
INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: DeleteSetting :exec
DELETE FROM settings WHERE key = $1;
