-- name: CountNewMatches :one
SELECT COUNT(*)::INT FROM matches
WHERE search_id = $1 AND status = 'new' AND deleted_at IS NULL;

-- Inserts the pair if it is not there yet. No row back means it already
-- was; TouchMatch then stamps it.
-- name: InsertMatch :one
INSERT INTO matches (id, search_id, listing_id)
VALUES ($1, $2, $3)
ON CONFLICT (search_id, listing_id) WHERE deleted_at IS NULL DO NOTHING
RETURNING *;

-- name: TouchMatch :one
UPDATE matches SET last_seen_at = NOW(), updated_at = NOW()
WHERE search_id = $1 AND listing_id = $2 AND deleted_at IS NULL
RETURNING *;

-- A search's matches with their listings, newest first. status NULL means
-- every status.
-- name: ListMatches :many
SELECT sqlc.embed(m), sqlc.embed(l)
FROM matches m
JOIN listings l ON l.id = m.listing_id AND l.deleted_at IS NULL
WHERE m.search_id = @search_id
  AND m.deleted_at IS NULL
  AND (sqlc.narg(status)::TEXT IS NULL OR m.status = sqlc.narg(status)::TEXT)
ORDER BY m.first_seen_at DESC, m.id DESC
LIMIT @limit_count;

-- Every search that found a listing, with the search's name.
-- name: ListMatchesForListing :many
SELECT sqlc.embed(m), s.name AS search_name
FROM matches m
JOIN searches s ON s.id = m.search_id AND s.deleted_at IS NULL
WHERE m.listing_id = $1 AND m.deleted_at IS NULL
ORDER BY m.first_seen_at;

-- name: SetMatchStatus :execrows
UPDATE matches SET status = @status, updated_at = NOW()
WHERE id = ANY(@ids::CHAR(20)[]) AND deleted_at IS NULL;
