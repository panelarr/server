-- name: InsertTask :one
-- Returns no row when a live task with the same dedupe key exists.
INSERT INTO tasks (kind, payload, dedupe_key, limit_key, priority, max_attempts, run_after, created_by)
VALUES (@kind, @payload, @dedupe_key, @limit_key, @priority, @max_attempts, @run_after, @created_by)
ON CONFLICT (dedupe_key) WHERE dedupe_key <> '' AND state IN ('queued', 'running') DO NOTHING
RETURNING *;

-- name: GetLiveTaskByDedupeKey :one
SELECT * FROM tasks WHERE dedupe_key = $1 AND state IN ('queued', 'running');

-- name: GetTask :one
SELECT * FROM tasks WHERE id = $1;

-- name: ClaimCandidates :many
-- Due tasks of the given kinds, best first, locked for this transaction;
-- the caller picks the ones its limits allow and marks them running.
SELECT * FROM tasks
WHERE state = 'queued' AND run_after <= now() AND kind = ANY(@kinds::text[])
ORDER BY priority DESC, run_after, created_at
LIMIT @lim
FOR UPDATE SKIP LOCKED;

-- name: StartTask :one
UPDATE tasks SET state = 'running', attempts = attempts + 1, lease_until = @lease_until,
    started_at = now(), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: ExtendTaskLease :exec
UPDATE tasks SET lease_until = @lease_until WHERE id = @id AND state = 'running';

-- name: SetTaskProgress :exec
UPDATE tasks SET progress = @progress, updated_at = now() WHERE id = @id AND state = 'running';

-- name: FinishTask :one
UPDATE tasks SET state = @state, last_error = @last_error, result = @result, lease_until = NULL,
    finished_at = now(), updated_at = now()
WHERE id = @id
RETURNING *;

-- name: RetryTaskLater :one
UPDATE tasks SET state = 'queued', last_error = @last_error, run_after = @run_after, lease_until = NULL,
    progress = NULL, updated_at = now()
WHERE id = @id
RETURNING *;

-- name: RequeueExpiredTasks :execrows
-- Running tasks whose lease ran out (the server stopped) go back to the
-- queue.
UPDATE tasks SET state = 'queued', lease_until = NULL, progress = NULL, updated_at = now(),
    last_error = 'interrupted'
WHERE state = 'running' AND (lease_until IS NULL OR lease_until < now());

-- name: RequeueTask :one
-- Retry by hand: a failed or cancelled task runs again with fresh attempts.
UPDATE tasks SET state = 'queued', attempts = 0, run_after = now(), last_error = '', progress = NULL,
    result = NULL, finished_at = NULL, updated_at = now()
WHERE id = @id AND state IN ('failed', 'cancelled')
RETURNING *;

-- name: CancelQueuedTask :one
UPDATE tasks SET state = 'cancelled', finished_at = now(), updated_at = now()
WHERE id = @id AND state = 'queued'
RETURNING *;

-- name: ListTasks :many
SELECT * FROM tasks
WHERE (sqlc.narg(kind)::text IS NULL OR kind = sqlc.narg(kind))
  AND (sqlc.narg(state)::text IS NULL OR state = sqlc.narg(state))
  AND (sqlc.narg(before_created)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(before_created)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY created_at DESC, id DESC
LIMIT @lim;

-- name: TaskStats :many
SELECT kind, state, count(*) AS n FROM tasks GROUP BY kind, state;

-- name: PruneFinishedTasks :execrows
DELETE FROM tasks WHERE state IN ('succeeded', 'cancelled') AND finished_at < @before;

-- name: UnclaimTask :one
-- Back to the queue without counting the attempt (the server stopped).
UPDATE tasks SET state = 'queued', attempts = greatest(attempts - 1, 0), run_after = now(),
    lease_until = NULL, progress = NULL, last_error = 'interrupted', updated_at = now()
WHERE id = $1
RETURNING *;

-- name: HurryTask :one
-- Asking again for queued work brings it forward (e.g. a refresh while a
-- retry waits out its back-off).
UPDATE tasks SET run_after = least(run_after, @run_after), priority = greatest(priority, @priority), updated_at = now()
WHERE dedupe_key = @dedupe_key AND state = 'queued'
RETURNING *;
