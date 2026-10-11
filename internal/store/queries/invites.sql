-- name: CreateInvite :one
INSERT INTO invites (token_hash, role, grants, note, created_by, expires_at)
VALUES (@token_hash, @role, @grants, @note, @created_by, @expires_at)
RETURNING *;

-- name: GetOpenInviteByTokenHash :one
SELECT * FROM invites
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now();

-- name: GetOpenInviteByTokenHashForUpdate :one
SELECT * FROM invites
WHERE token_hash = $1 AND used_at IS NULL AND expires_at > now()
FOR UPDATE;

-- name: MarkInviteUsed :exec
UPDATE invites SET used_at = now(), used_by = @used_by WHERE id = @id;

-- name: ListOpenInvites :many
SELECT * FROM invites WHERE used_at IS NULL AND expires_at > now() ORDER BY created_at DESC, id;

-- name: DeleteInvite :execrows
DELETE FROM invites WHERE id = $1;
