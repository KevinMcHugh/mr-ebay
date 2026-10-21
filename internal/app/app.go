// Package app holds what every surface (REST, MCP, web pages) shares: the
// Store, the errors the surfaces map to their own responses, and Links.
// Domain rules that more than one endpoint needs live here too.
package app

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/xid"

	"github.com/KevinMcHugh/mr-ebay/internal/auth"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
)

// Name is the app's display name, on the pages, the OAuth authorize page
// and the MCP server's implementation info.
const Name = "Mr. eBay"

// Errors the surfaces (REST, MCP, web) map to their own responses: REST to
// 404, 422 and 502, the pages to the same statuses, and MCP to a tool
// error.
var (
	ErrNotFound = errors.New("not found")
	ErrBadInput = errors.New("bad input")
	// ErrUpstream is eBay failing, refusing, or not being configured.
	ErrUpstream = errors.New("eBay unavailable")
)

// MaxBodyBytes bounds a request body.
const MaxBodyBytes = 1 << 20

// Store is the app's persistence: every sqlc query, plus the operations that
// need a transaction. Endpoints each declare a narrow interface of just the
// methods they call, and *Store satisfies all of them.
type Store struct {
	*dbgen.Queries
	Pool *pgxpool.Pool
}

// NewStore builds a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Queries: dbgen.New(pool), Pool: pool}
}

// RedeemInvite spends a single-use invite: it mints the newcomer's api key
// (named after the invite's note) and runs approve with that key on the same
// transaction, so an invite is only used up if what it was redeemed for
// (an OAuth code) is stored too. An unknown, used, expired or deleted invite
// is pgx.ErrNoRows. The raw key is never shown to anyone: the newcomer acts
// through the OAuth tokens it approves, and deleting the key cuts them off.
func (s *Store) RedeemInvite(ctx context.Context, raw string, approve func(q *dbgen.Queries, apiKeyID string) error) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	q := s.WithTx(tx)
	inv, err := q.ClaimInviteByHash(ctx, auth.Hash(raw))
	if err != nil {
		return err
	}
	name := inv.Note
	if name == "" {
		name = "invite " + inv.ID
	}
	key, err := auth.GenerateKey()
	if err != nil {
		return err
	}
	row, err := q.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{ID: xid.New().String(), Name: name, KeyHash: auth.Hash(key)})
	if err != nil {
		return err
	}
	if err := q.SetInviteKey(ctx, dbgen.SetInviteKeyParams{ID: inv.ID, ApiKeyID: &row.ID}); err != nil {
		return err
	}
	if err := approve(q, row.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Upstream marks an eBay client error as ErrUpstream, keeping the client's
// own error (ebay.ErrNotConfigured, ebay.ErrRateLimited, *ebay.APIError)
// for errors.Is and errors.As.
func Upstream(err error) error {
	return fmt.Errorf("%w: %w", ErrUpstream, err)
}

// Recorded is what RecordResults did with one item from eBay.
type Recorded struct {
	Listing dbgen.Listing
	Match   dbgen.Match
	// NewMatch is true when this search had not found the listing before.
	NewMatch bool
	// PrevTotalCents is the listing's total (price plus shipping) before
	// this run, when it changed; nil when it is unchanged or first seen.
	PrevTotalCents *int64
}

// RecordResults stores one run's items for a search, in one transaction:
// each listing is upserted by its eBay item id, a price point is added when
// its price or shipping differs from the last one seen (or it is new), and
// the search-listing match is created or stamped. A match keeps its status,
// so a dismissed listing stays dismissed when it turns up again.
func (s *Store) RecordResults(ctx context.Context, searchID string, items []ebay.Item) ([]Recorded, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	q := s.WithTx(tx)
	out := make([]Recorded, 0, len(items))
	for _, it := range items {
		var ends pgtype.Timestamptz
		if it.EndsAt != nil {
			ends = pgtype.Timestamptz{Time: *it.EndsAt, Valid: true}
		}
		l, err := q.UpsertListing(ctx, dbgen.UpsertListingParams{
			ID: xid.New().String(), EbayItemID: it.ItemID, Marketplace: it.Marketplace,
			Title: it.Title, Url: it.URL, ImageUrl: it.ImageURL, Seller: it.Seller,
			SellerFeedbackPct: it.SellerFeedbackPct, Condition: it.Condition, BuyingOptions: it.BuyingOptions,
			PriceCents: it.PriceCents, ShippingCents: it.ShippingCents, Currency: it.Currency,
			EndsAt: ends, Data: it.Raw,
		})
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", it.ItemID, err)
		}
		rec := Recorded{Listing: l}

		last, err := q.LatestPricePoint(ctx, l.ID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = q.InsertPricePoint(ctx, dbgen.InsertPricePointParams{ID: xid.New().String(), ListingID: l.ID, PriceCents: it.PriceCents, ShippingCents: it.ShippingCents, Currency: it.Currency})
		case err != nil:
		case last.PriceCents != it.PriceCents || !sameCents(last.ShippingCents, it.ShippingCents) || last.Currency != it.Currency:
			prev := last.PriceCents + deref(last.ShippingCents)
			rec.PrevTotalCents = &prev
			err = q.InsertPricePoint(ctx, dbgen.InsertPricePointParams{ID: xid.New().String(), ListingID: l.ID, PriceCents: it.PriceCents, ShippingCents: it.ShippingCents, Currency: it.Currency})
		}
		if err != nil {
			return nil, fmt.Errorf("price of %s: %w", it.ItemID, err)
		}

		rec.Match, err = q.InsertMatch(ctx, dbgen.InsertMatchParams{ID: xid.New().String(), SearchID: searchID, ListingID: l.ID})
		rec.NewMatch = err == nil
		if errors.Is(err, pgx.ErrNoRows) {
			rec.Match, err = q.TouchMatch(ctx, dbgen.TouchMatchParams{SearchID: searchID, ListingID: l.ID})
		}
		if err != nil {
			return nil, fmt.Errorf("match for %s: %w", it.ItemID, err)
		}
		out = append(out, rec)
	}
	return out, tx.Commit(ctx)
}

func sameCents(a, b *int64) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// Links builds the absolute URLs that view models carry.
type Links struct {
	// Base is the app's public origin ("https://mr-ebay-xyz.sprites.app").
	// Empty gives relative links.
	Base string
}

// NewLinks trims base for joining.
func NewLinks(base string) Links { return Links{Base: strings.TrimRight(base, "/")} }

// Listing is a stored listing's page.
func (l Links) Listing(id string) string { return l.Base + "/listings/" + id }

// Search is a saved search's page.
func (l Links) Search(id string) string { return l.Base + "/searches/" + id }

// NotFound turns pgx's no-rows into ErrNotFound, naming what was missing.
func NotFound(err error, what, id string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s %s", ErrNotFound, what, id)
	}
	return err
}
