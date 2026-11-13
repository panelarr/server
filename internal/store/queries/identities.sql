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

-- name: ListUserIdentities :many
SELECT * FROM user_identities WHERE user_id = $1 ORDER BY created_at, id;

-- name: CountUserIdentitiesAtIssuer :one
SELECT count(*) FROM user_identities WHERE user_id = @user_id AND issuer = @issuer;

-- name: DeleteUserIdentity :execrows
DELETE FROM user_identities WHERE id = @id AND user_id = @user_id;

-- name: UsersWithIdentities :many
-- Which of the given users have a linked login.
SELECT DISTINCT user_id FROM user_identities WHERE user_id = ANY(@user_ids::uuid[]);
