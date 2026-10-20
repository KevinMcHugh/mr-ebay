-- name: CountNewMatches :one
SELECT COUNT(*)::INT FROM matches
WHERE search_id = $1 AND status = 'new' AND deleted_at IS NULL;
