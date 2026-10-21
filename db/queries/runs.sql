-- name: CreateRun :one
INSERT INTO search_runs (id, search_id, trigger)
VALUES ($1, $2, $3)
RETURNING *;

-- name: FinishRun :one
UPDATE search_runs SET
    finished_at = NOW(),
    result_count = $2,
    new_count = $3,
    price_drop_count = $4,
    error = $5
WHERE id = $1
RETURNING *;

-- name: MarkSearchRun :exec
UPDATE searches SET last_run_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;

-- Newest first, for one search or (search_id NULL) every live one.
-- name: ListRuns :many
SELECT r.* FROM search_runs r
JOIN searches s ON s.id = r.search_id AND s.deleted_at IS NULL
WHERE (sqlc.narg(search_id)::CHAR(20) IS NULL OR r.search_id = sqlc.narg(search_id)::CHAR(20))
ORDER BY r.started_at DESC, r.id DESC
LIMIT @limit_count;
