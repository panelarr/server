-- name: CreateUserIdentity :one
INSERT INTO user_identities (user_id, issuer, subject, email)
VALUES (@user_id, @issuer, @subject, @email)
RETURNING *;

-- name: GetUserByIdentity :one
-- The user a provider login is linked to, by issuer and subject only.
SELECT sqlc.embed(user_identities), sqlc.embed(users)
FROM user_identities JOIN users ON users.id = user_identities.user_id
WHERE user_identities.issuer = @issuer AND user_identities.subject = @subject;

-- name: TouchUserIdentity :exec
UPDATE user_identities SET last_login_at = now(), email = @email WHERE id = @id;
