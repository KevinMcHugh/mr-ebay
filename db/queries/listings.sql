-- One row per eBay item: a run refreshes everything eBay says about it and
-- stamps last_seen_at.
-- name: UpsertListing :one
INSERT INTO listings (
    id, ebay_item_id, marketplace, title, url, image_url, seller,
    seller_feedback_pct, condition, buying_options, price_cents,
    shipping_cents, currency, ends_at, data
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
ON CONFLICT (ebay_item_id) WHERE deleted_at IS NULL DO UPDATE SET
    marketplace = EXCLUDED.marketplace,
    title = EXCLUDED.title,
    url = EXCLUDED.url,
    image_url = EXCLUDED.image_url,
    seller = EXCLUDED.seller,
    seller_feedback_pct = EXCLUDED.seller_feedback_pct,
    condition = EXCLUDED.condition,
    buying_options = EXCLUDED.buying_options,
    price_cents = EXCLUDED.price_cents,
    shipping_cents = EXCLUDED.shipping_cents,
    currency = EXCLUDED.currency,
    ends_at = EXCLUDED.ends_at,
    data = EXCLUDED.data,
    last_seen_at = NOW(),
    updated_at = NOW()
RETURNING *;

-- name: GetListing :one
SELECT * FROM listings
WHERE id = $1 AND deleted_at IS NULL;

-- name: LatestPricePoint :one
SELECT * FROM price_points
WHERE listing_id = $1
ORDER BY observed_at DESC, id DESC
LIMIT 1;

-- name: InsertPricePoint :exec
INSERT INTO price_points (id, listing_id, price_cents, shipping_cents, currency)
VALUES ($1, $2, $3, $4, $5);

-- Newest first.
-- name: ListPricePoints :many
SELECT * FROM price_points
WHERE listing_id = $1
ORDER BY observed_at DESC, id DESC;

-- name: ForgetSeller :execrows
-- An eBay user closed their account (Marketplace Account Deletion), so
-- nothing of theirs may stay: their username, their feedback, and the seller
-- object in eBay's raw summary. The listing itself is about the item, not
-- the person, so it stays. Unlike every other query this one includes
-- soft-deleted rows, because a deleted row still holds the data.
UPDATE listings
SET seller = '', seller_feedback_pct = NULL, data = data - 'seller', updated_at = NOW()
WHERE seller = sqlc.arg(seller)::text;
