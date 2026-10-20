-- name: CreateSearch :one
INSERT INTO searches (
    id, name, query, category_ids, conditions, buying_options,
    min_price_cents, max_price_cents, currency, marketplace, wants,
    watch, alert_below_cents, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetSearch :one
SELECT * FROM searches
WHERE id = $1 AND deleted_at IS NULL;

-- Newest first, with how many matches are still new.
-- name: ListSearches :many
SELECT searches.*,
       (SELECT COUNT(*) FROM matches m
         WHERE m.search_id = searches.id AND m.status = 'new'
           AND m.deleted_at IS NULL)::INT AS new_count
FROM searches
WHERE deleted_at IS NULL
ORDER BY created_at DESC;

-- Replaces every editable field. The endpoint merges a partial update onto
-- the current row first, because a NULL here has to be able to mean "no
-- limit" for the price columns.
-- name: UpdateSearch :one
UPDATE searches SET
    name = $2, query = $3, category_ids = $4, conditions = $5,
    buying_options = $6, min_price_cents = $7, max_price_cents = $8,
    currency = $9, marketplace = $10, wants = $11, watch = $12,
    alert_below_cents = $13, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: DeleteSearch :execrows
UPDATE searches SET deleted_at = NOW(), updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL;
