-- name: ListSolvers :many
SELECT * FROM network_solvers ORDER BY lower(name), id;

-- name: GetSolver :one
SELECT * FROM network_solvers WHERE id = $1;

-- name: InsertSolver :one
INSERT INTO network_solvers (id, name, type, url, session_ttl_seconds, max_timeout_seconds, as_response_fallback, owner_kind, owner_id, owner_plugin)
VALUES (@id, @name, @type, @url, @session_ttl_seconds, @max_timeout_seconds, @as_response_fallback, @owner_kind, @owner_id, @owner_plugin)
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
INSERT INTO network_proxies (id, name, url, username, password_sealed, owner_kind, owner_id, owner_plugin)
VALUES (@id, @name, @url, @username, @password_sealed, @owner_kind, @owner_id, @owner_plugin)
RETURNING *;

-- name: UpdateProxy :one
UPDATE network_proxies SET name = @name, url = @url, username = @username, password_sealed = @password_sealed,
    updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteProxy :execrows
DELETE FROM network_proxies WHERE id = $1;

-- name: ListProfiles :many
SELECT * FROM network_profiles ORDER BY lower(name), id;

-- name: GetProfile :one
SELECT * FROM network_profiles WHERE id = $1;

-- name: InsertProfile :one
INSERT INTO network_profiles (
    id, name, proxy_id, solver_id, rate_requests, rate_period_ms, rate_burst, rate_scope, concurrency,
    user_agent_mode, user_agent, doh_url, headers, shared_cookie_jar, connect_timeout_ms, read_timeout_ms,
    call_timeout_ms, owner_kind, owner_id, owner_plugin
) VALUES (
    @id, @name, @proxy_id, @solver_id, @rate_requests, @rate_period_ms, @rate_burst, @rate_scope, @concurrency,
    @user_agent_mode, @user_agent, @doh_url, @headers, @shared_cookie_jar, @connect_timeout_ms, @read_timeout_ms,
    @call_timeout_ms, @owner_kind, @owner_id, @owner_plugin
)
RETURNING *;

-- name: UpdateProfile :one
UPDATE network_profiles SET
    name = @name, proxy_id = @proxy_id, solver_id = @solver_id, rate_requests = @rate_requests,
    rate_period_ms = @rate_period_ms, rate_burst = @rate_burst, rate_scope = @rate_scope, concurrency = @concurrency,
    user_agent_mode = @user_agent_mode, user_agent = @user_agent, doh_url = @doh_url, headers = @headers,
    shared_cookie_jar = @shared_cookie_jar, connect_timeout_ms = @connect_timeout_ms,
    read_timeout_ms = @read_timeout_ms, call_timeout_ms = @call_timeout_ms, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: DeleteProfile :execrows
DELETE FROM network_profiles WHERE id = $1;

-- name: ListAssignments :many
SELECT * FROM network_assignments ORDER BY target_kind, target_id;

-- name: UpsertAssignment :one
INSERT INTO network_assignments (target_kind, target_id, profile_id, plugin_id)
VALUES (@target_kind, @target_id, @profile_id, @plugin_id)
ON CONFLICT (target_kind, target_id) DO UPDATE SET
    profile_id = EXCLUDED.profile_id, plugin_id = EXCLUDED.plugin_id, updated_at = now()
RETURNING *;

-- name: DeleteAssignment :execrows
DELETE FROM network_assignments WHERE target_kind = @target_kind AND target_id = @target_id;

-- name: ListPushes :many
SELECT * FROM network_pushes WHERE target_kind = @target_kind;

-- name: UpsertPush :exec
INSERT INTO network_pushes (target_kind, target_id, plugin_id, hash, error, pushed_at)
VALUES (@target_kind, @target_id, @plugin_id, @hash, @error, now())
ON CONFLICT (target_kind, target_id) DO UPDATE SET
    plugin_id = EXCLUDED.plugin_id, hash = EXCLUDED.hash, error = EXCLUDED.error, pushed_at = now();
