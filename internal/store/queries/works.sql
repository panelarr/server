-- name: UpsertWork :one
INSERT INTO works (meta_entry_id, title, sort_title, type, status, content_rating, year, original_language, cover_url)
VALUES (@meta_entry_id, @title, @sort_title, @type, @status, @content_rating, @year, @original_language, @cover_url)
ON CONFLICT (meta_entry_id) DO UPDATE SET
    title = EXCLUDED.title, sort_title = EXCLUDED.sort_title, type = EXCLUDED.type, status = EXCLUDED.status,
    content_rating = EXCLUDED.content_rating, year = EXCLUDED.year, original_language = EXCLUDED.original_language,
    cover_url = EXCLUDED.cover_url, meta_synced_at = now(), updated_at = now()
RETURNING *, (xmax = 0) AS inserted;

-- name: GetWork :one
SELECT * FROM works WHERE id = $1;

-- name: GetWorkByEntry :one
SELECT * FROM works WHERE meta_entry_id = $1;

-- name: AddLibraryEntry :one
INSERT INTO library_entries (user_id, work_id) VALUES (@user_id, @work_id)
ON CONFLICT DO NOTHING
RETURNING *;

-- name: GetLibraryEntry :one
SELECT * FROM library_entries WHERE user_id = @user_id AND work_id = @work_id;

-- name: RemoveLibraryEntry :execrows
DELETE FROM library_entries WHERE user_id = @user_id AND work_id = @work_id;

-- name: LibraryUserIDs :many
SELECT user_id FROM library_entries WHERE work_id = $1;

-- name: ListLibrary :many
-- A user's library by title, after a (sort_title, id) cursor. Content
-- filters run in Go (they depend on the user's grants).
SELECT sqlc.embed(works), library_entries.added_at
FROM library_entries JOIN works ON works.id = library_entries.work_id
WHERE library_entries.user_id = @user_id
  AND (sqlc.narg(query)::text IS NULL OR works.title ILIKE '%' || sqlc.narg(query) || '%')
  AND (sqlc.narg(after_title)::text IS NULL OR (works.sort_title, works.id) > (sqlc.narg(after_title)::text, sqlc.narg(after_id)::uuid))
ORDER BY works.sort_title, works.id
LIMIT @lim;

-- name: UpsertSourceSeries :one
-- Records a series found for a work. An attached or rejected series keeps
-- its state when matching finds it again.
INSERT INTO source_series (work_id, source_id, key, title, url, cover_ref, state, origin, score, evidence)
VALUES (@work_id, @source_id, @key, @title, @url, @cover_ref, @state, @origin, @score, @evidence)
ON CONFLICT (work_id, source_id, key) DO UPDATE SET
    title = EXCLUDED.title, url = EXCLUDED.url, cover_ref = EXCLUDED.cover_ref,
    score = EXCLUDED.score, evidence = EXCLUDED.evidence, updated_at = now()
RETURNING *;

-- name: GetSourceSeries :one
SELECT * FROM source_series WHERE id = $1;

-- name: ListSourceSeries :many
SELECT * FROM source_series WHERE work_id = $1 ORDER BY source_id, state, score DESC, id;

-- name: AttachedSourceSeries :many
SELECT * FROM source_series WHERE work_id = $1 AND state = 'attached' ORDER BY priority DESC, source_id;

-- name: SetSourceSeriesState :one
UPDATE source_series SET state = @state, origin = @origin, updated_at = now() WHERE id = @id
RETURNING *;

-- name: DemoteAttached :exec
-- Before attaching another series of the same source.
UPDATE source_series SET state = 'candidate', updated_at = now()
WHERE work_id = @work_id AND source_id = @source_id AND state = 'attached';

-- name: DropCandidates :exec
-- Candidates of a source are replaced on every matching run.
DELETE FROM source_series WHERE work_id = @work_id AND source_id = @source_id AND state = 'candidate';

-- name: SetSourcePriority :execrows
UPDATE source_series SET priority = @priority, updated_at = now() WHERE id = @id AND work_id = @work_id;

-- name: UpsertWorkSourceMatch :exec
INSERT INTO work_source_matches (work_id, source_id, status, message, queries, checked_at)
VALUES (@work_id, @source_id, @status, @message, @queries, now())
ON CONFLICT (work_id, source_id) DO UPDATE SET
    status = EXCLUDED.status, message = EXCLUDED.message, queries = EXCLUDED.queries, checked_at = now();

-- name: ListWorkSourceMatches :many
SELECT * FROM work_source_matches WHERE work_id = $1 ORDER BY source_id;

-- name: CountAttachedByWork :many
SELECT work_id, count(*) AS attached FROM source_series
WHERE state = 'attached' AND work_id = ANY(@work_ids::uuid[]) GROUP BY work_id;

-- name: AttachedSourceIDsByWork :many
SELECT work_id, source_id FROM source_series
WHERE state = 'attached' AND work_id = ANY(@work_ids::uuid[]);
