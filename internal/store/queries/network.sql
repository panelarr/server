-- name: ListSolvers :many
SELECT * FROM network_solvers ORDER BY lower(name), id;

-- name: GetSolver :one
SELECT * FROM network_solvers WHERE id = $1;

-- name: InsertSolver :one
INSERT INTO network_solvers (id, name, type, url, session_ttl_seconds, max_timeout_seconds, as_response_fallback)
VALUES (@id, @name, @type, @url, @session_ttl_seconds, @max_timeout_seconds, @as_response_fallback)
RETURNING *;

-- name: UpdateSolver :one
UPDATE network_solvers SET name = @name, type = @type, url = @url, session_ttl_seconds = @session_ttl_seconds,
    max_timeout_seconds = @max_timeout_seconds, as_response_fallback = @as_response_fallback, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteSolver :execrows
DELETE FROM network_solvers WHERE id = $1;

-- name: ListProxies :many
SELECT * FROM network_proxies ORDER BY lower(name), id;

-- name: GetProxy :one
SELECT * FROM network_proxies WHERE id = $1;

-- name: InsertProxy :one
INSERT INTO network_proxies (id, name, url, username, password_sealed)
VALUES (@id, @name, @url, @username, @password_sealed)
RETURNING *;

-- name: UpdateProxy :one
UPDATE network_proxies SET name = @name, url = @url, username = @username, password_sealed = @password_sealed,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteProxy :execrows
DELETE FROM network_proxies WHERE id = $1;
