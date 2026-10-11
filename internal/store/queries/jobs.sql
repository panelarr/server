-- name: CreateJobRun :one
INSERT INTO job_runs (job_id, status, trigger, triggered_by, params, message, finished_at)
VALUES (@job_id, @status, @trigger, @triggered_by, @params, @message, @finished_at)
RETURNING *;

-- name: FinishJobRun :one
UPDATE job_runs SET status = @status, message = @message, counts = @counts, notes = @notes,
    progress = NULL, finished_at = now()
WHERE id = @id
RETURNING *;

-- name: SetJobRunProgress :exec
UPDATE job_runs SET progress = @progress, counts = @counts, notes = @notes WHERE id = @id AND status = 'running';

-- name: GetJobRun :one
SELECT * FROM job_runs WHERE id = $1;

-- name: ListJobRuns :many
SELECT * FROM job_runs
WHERE job_id = @job_id
  AND (sqlc.narg(status)::text IS NULL OR status = sqlc.narg(status))
  AND (sqlc.narg(before_started)::timestamptz IS NULL
       OR (started_at, id) < (sqlc.narg(before_started)::timestamptz, sqlc.narg(before_id)::uuid))
ORDER BY started_at DESC, id DESC
LIMIT @lim;

-- name: LastJobRuns :many
SELECT DISTINCT ON (job_id) * FROM job_runs ORDER BY job_id, started_at DESC, id DESC;

-- name: FailInterruptedJobRuns :execrows
UPDATE job_runs SET status = 'failed', message = 'interrupted', progress = NULL, finished_at = now()
WHERE status = 'running';

-- name: PruneJobRuns :execrows
DELETE FROM job_runs r
WHERE r.job_id = @job_id AND r.status <> 'running' AND r.id NOT IN (
    SELECT id FROM job_runs WHERE job_id = @job_id ORDER BY started_at DESC, id DESC LIMIT @keep
);
