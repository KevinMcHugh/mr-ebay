-- migrate:up

-- The eBay side of the app (docs/design.md). Prices are integer minor units
-- (cents) with a currency, never floats.

-- A saved search: what to look for, and whether to watch it unattended.
-- The filter columns map onto the Browse API's item_summary/search
-- parameters; wants is the same thing in words, for the judgment calls a
-- filter cannot make ("a 15-inch laptop, not a 13").
CREATE TABLE searches (
    id                CHAR(20)    PRIMARY KEY,
    name              TEXT        NOT NULL,
    query             TEXT        NOT NULL,
    category_ids      TEXT[]      NOT NULL DEFAULT '{}',
    conditions        TEXT[]      NOT NULL DEFAULT '{}',  -- NEW, USED, UNSPECIFIED
    buying_options    TEXT[]      NOT NULL DEFAULT '{}',  -- FIXED_PRICE, AUCTION, BEST_OFFER
    min_price_cents   BIGINT,
    max_price_cents   BIGINT,
    currency          TEXT        NOT NULL DEFAULT 'USD',
    marketplace       TEXT        NOT NULL DEFAULT 'EBAY_US',
    wants             TEXT        NOT NULL DEFAULT '',
    watch             BOOLEAN     NOT NULL DEFAULT FALSE,
    -- A watched search alerts on a match at or below this total (price plus
    -- shipping). NULL alerts on every new match.
    alert_below_cents BIGINT,
    last_run_at       TIMESTAMPTZ,
    created_by        CHAR(20)    REFERENCES api_keys(id),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ,
    CHECK (min_price_cents IS NULL OR max_price_cents IS NULL OR min_price_cents <= max_price_cents)
);

CREATE INDEX searches_active ON searches (created_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX searches_watched ON searches (last_run_at NULLS FIRST) WHERE watch AND deleted_at IS NULL;

-- One eBay listing, as last seen. Several searches can find the same one,
-- so it is stored once, keyed by eBay's item id. data keeps the item
-- summary exactly as eBay returned it, so a field we did not copy out is
-- never lost.
CREATE TABLE listings (
    id                  CHAR(20)         PRIMARY KEY,
    ebay_item_id        TEXT             NOT NULL,
    marketplace         TEXT             NOT NULL,
    title               TEXT             NOT NULL,
    url                 TEXT             NOT NULL,
    image_url           TEXT             NOT NULL DEFAULT '',
    seller              TEXT             NOT NULL DEFAULT '',
    seller_feedback_pct DOUBLE PRECISION,
    condition           TEXT             NOT NULL DEFAULT '',
    buying_options      TEXT[]           NOT NULL DEFAULT '{}',
    price_cents         BIGINT           NOT NULL,
    shipping_cents      BIGINT,          -- NULL when eBay did not say
    currency            TEXT             NOT NULL,
    ends_at             TIMESTAMPTZ,
    first_seen_at       TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    last_seen_at        TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    data                JSONB            NOT NULL,
    created_at          TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ      NOT NULL DEFAULT NOW(),
    deleted_at          TIMESTAMPTZ
);

CREATE UNIQUE INDEX listings_item ON listings (ebay_item_id) WHERE deleted_at IS NULL;

-- Which search found which listing, and what you made of it there. status
-- is per search: dismissing a listing from one search leaves it alone in
-- another.
CREATE TABLE matches (
    id            CHAR(20)    PRIMARY KEY,
    search_id     CHAR(20)    NOT NULL REFERENCES searches(id),
    listing_id    CHAR(20)    NOT NULL REFERENCES listings(id),
    status        TEXT        NOT NULL DEFAULT 'new'
                  CHECK (status IN ('new', 'seen', 'starred', 'dismissed')),
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    alerted_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE UNIQUE INDEX matches_pair ON matches (search_id, listing_id) WHERE deleted_at IS NULL;
CREATE INDEX matches_by_status ON matches (search_id, status, first_seen_at DESC) WHERE deleted_at IS NULL;

-- A listing's price history: a row whenever a run sees a different total
-- than the last one. Append-only history, so like a log it has no
-- updated_at or deleted_at; it goes when its listing does.
CREATE TABLE price_points (
    id             CHAR(20)    PRIMARY KEY,
    listing_id     CHAR(20)    NOT NULL REFERENCES listings(id),
    price_cents    BIGINT      NOT NULL,
    shipping_cents BIGINT,
    currency       TEXT        NOT NULL,
    observed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX price_points_listing ON price_points (listing_id, observed_at DESC);

-- One execution of a search, by hand or on the watch schedule. Append-only,
-- like price_points: it is what the "is the watcher actually running?"
-- question is answered from.
CREATE TABLE search_runs (
    id                CHAR(20)    PRIMARY KEY,
    search_id         CHAR(20)    NOT NULL REFERENCES searches(id),
    trigger           TEXT        NOT NULL CHECK (trigger IN ('manual', 'watch')),
    started_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at       TIMESTAMPTZ,
    result_count      INT         NOT NULL DEFAULT 0,
    new_count         INT         NOT NULL DEFAULT 0,
    price_drop_count  INT         NOT NULL DEFAULT 0,
    error             TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX search_runs_recent ON search_runs (search_id, started_at DESC);

-- migrate:down

DROP TABLE search_runs;
DROP TABLE price_points;
DROP TABLE matches;
DROP TABLE listings;
DROP TABLE searches;
