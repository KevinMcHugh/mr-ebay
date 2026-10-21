// Package listings holds the Listing view model every surface shows, the
// ad-hoc eBay search (which stores nothing), and the listing detail.
package listings

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// Ad-hoc search limits.
const (
	DefaultAdHocItems = 50
	MaxAdHocItems     = 200
)

// -----------------------------------------------------------------------------
// View model
// -----------------------------------------------------------------------------

// Listing is one eBay listing as every surface shows it. ID, FirstSeenAt,
// LastSeenAt and PageURL are empty for an ad-hoc result, which is not
// stored.
type Listing struct {
	ID                string     `json:"id,omitempty"`
	EbayItemID        string     `json:"ebayItemId"`
	Title             string     `json:"title"`
	URL               string     `json:"url"`
	ImageURL          string     `json:"imageUrl"`
	Seller            string     `json:"seller"`
	SellerFeedbackPct *float64   `json:"sellerFeedbackPct,omitempty"`
	Condition         string     `json:"condition"`
	BuyingOptions     []string   `json:"buyingOptions"`
	PriceCents        int64      `json:"priceCents"`
	ShippingCents     *int64     `json:"shippingCents,omitempty"`
	TotalCents        int64      `json:"totalCents"`
	Currency          string     `json:"currency"`
	Marketplace       string     `json:"marketplace"`
	EndsAt            *time.Time `json:"endsAt,omitempty"`
	FirstSeenAt       *time.Time `json:"firstSeenAt,omitempty"`
	LastSeenAt        *time.Time `json:"lastSeenAt,omitempty"`
	PageURL           string     `json:"pageUrl,omitempty"`
}

// FromItem is an unsaved eBay item.
func FromItem(it ebay.Item) Listing {
	return Listing{
		EbayItemID: it.ItemID, Title: it.Title, URL: it.URL, ImageURL: it.ImageURL,
		Seller: it.Seller, SellerFeedbackPct: it.SellerFeedbackPct, Condition: it.Condition,
		BuyingOptions: it.BuyingOptions, PriceCents: it.PriceCents, ShippingCents: it.ShippingCents,
		TotalCents: it.TotalCents(), Currency: it.Currency, Marketplace: it.Marketplace, EndsAt: it.EndsAt,
	}
}

// ToViewModel is a stored listing.
func ToViewModel(l dbgen.Listing, links app.Links) Listing {
	return Listing{
		ID: l.ID, EbayItemID: l.EbayItemID, Title: l.Title, URL: l.Url, ImageURL: l.ImageUrl,
		Seller: l.Seller, SellerFeedbackPct: l.SellerFeedbackPct, Condition: l.Condition,
		BuyingOptions: l.BuyingOptions, PriceCents: l.PriceCents, ShippingCents: l.ShippingCents,
		TotalCents: l.PriceCents + Deref(l.ShippingCents), Currency: l.Currency, Marketplace: l.Marketplace,
		EndsAt: TimePtr(l.EndsAt), FirstSeenAt: TimePtr(l.FirstSeenAt), LastSeenAt: TimePtr(l.LastSeenAt),
		PageURL: links.Listing(l.ID),
	}
}

// ToAPI is l as the API type.
func ToAPI(l Listing) apigen.Listing {
	return apigen.Listing{
		Id: l.ID, EbayItemId: l.EbayItemID, Title: l.Title, Url: l.URL, ImageUrl: l.ImageURL,
		Seller: l.Seller, SellerFeedbackPct: l.SellerFeedbackPct, Condition: l.Condition,
		BuyingOptions: l.BuyingOptions, PriceCents: l.PriceCents, ShippingCents: l.ShippingCents,
		TotalCents: l.TotalCents, Currency: l.Currency, Marketplace: l.Marketplace,
		EndsAt: l.EndsAt, FirstSeenAt: l.FirstSeenAt, LastSeenAt: l.LastSeenAt, PageUrl: l.PageURL,
	}
}

// PricePoint is one entry in a listing's price history.
type PricePoint struct {
	PriceCents    int64     `json:"priceCents"`
	ShippingCents *int64    `json:"shippingCents,omitempty"`
	TotalCents    int64     `json:"totalCents"`
	Currency      string    `json:"currency"`
	ObservedAt    time.Time `json:"observedAt"`
}

// FoundBy is a search that found a listing, and the listing's status there.
type FoundBy struct {
	MatchID    string `json:"matchId"`
	SearchID   string `json:"searchId"`
	SearchName string `json:"searchName"`
	Status     string `json:"status"`
}

// Detail is a stored listing with its price history (newest first) and the
// searches that found it.
type Detail struct {
	Listing      Listing      `json:"listing"`
	PriceHistory []PricePoint `json:"priceHistory"`
	Matches      []FoundBy    `json:"matches"`
}

// -----------------------------------------------------------------------------
// Ad-hoc search
// -----------------------------------------------------------------------------

// Searcher is the eBay client.
type Searcher interface {
	Search(ctx context.Context, q ebay.Query) (ebay.Result, error)
}

// AdHoc is an unsaved search's results.
type AdHoc struct {
	Total    int       `json:"total"`
	Listings []Listing `json:"listings"`
}

// SearchEndpoint runs a query against eBay and stores nothing, so Claude
// can try queries before saving one. Its filters are checked exactly as a
// saved search's are.
type SearchEndpoint struct{ Ebay Searcher }

func (e SearchEndpoint) Interact(ctx context.Context, req apigen.SearchEbayRequestObject) (ebay.Result, error) {
	p := req.Params
	f, err := searches.CleanQuery(searches.Fields{
		Query: p.Q, CategoryIDs: p.CategoryIds, Conditions: p.Conditions, BuyingOptions: p.BuyingOptions,
		MinPriceCents: p.MinPriceCents, MaxPriceCents: p.MaxPriceCents, Currency: p.Currency, Marketplace: p.Marketplace,
	})
	if err != nil {
		return ebay.Result{}, err
	}
	n := p.MaxItems
	if n <= 0 {
		n = DefaultAdHocItems
	}
	res, err := e.Ebay.Search(ctx, f.EbayQuery(min(n, MaxAdHocItems)))
	if err != nil {
		return ebay.Result{}, app.Upstream(err)
	}
	return res, nil
}

func (SearchEndpoint) Build(res ebay.Result) AdHoc {
	out := AdHoc{Total: res.Total, Listings: make([]Listing, len(res.Items))}
	for i, it := range res.Items {
		out.Listings[i] = FromItem(it)
	}
	return out
}

func (SearchEndpoint) Render(vm AdHoc) apigen.SearchEbayResponseObject {
	out := make([]apigen.Listing, len(vm.Listings))
	for i, l := range vm.Listings {
		out[i] = ToAPI(l)
	}
	return apigen.SearchEbay200JSONResponse{Total: vm.Total, Listings: out}
}

// -----------------------------------------------------------------------------
// Get listing
// -----------------------------------------------------------------------------

type GetStore interface {
	GetListing(ctx context.Context, id string) (dbgen.Listing, error)
	ListPricePoints(ctx context.Context, listingID string) ([]dbgen.PricePoint, error)
	ListMatchesForListing(ctx context.Context, listingID string) ([]dbgen.ListMatchesForListingRow, error)
}

// GetEndpoint is one stored listing in full.
type GetEndpoint struct {
	Store GetStore
	Links app.Links
}

type detailModel struct {
	Listing dbgen.Listing
	Prices  []dbgen.PricePoint
	Matches []dbgen.ListMatchesForListingRow
}

func (e GetEndpoint) Interact(ctx context.Context, req apigen.GetListingRequestObject) (detailModel, error) {
	l, err := e.Store.GetListing(ctx, req.ListingId)
	if err != nil {
		return detailModel{}, app.NotFound(err, "listing", req.ListingId)
	}
	prices, err := e.Store.ListPricePoints(ctx, l.ID)
	if err != nil {
		return detailModel{}, err
	}
	ms, err := e.Store.ListMatchesForListing(ctx, l.ID)
	return detailModel{Listing: l, Prices: prices, Matches: ms}, err
}

func (e GetEndpoint) Build(m detailModel) Detail {
	d := Detail{Listing: ToViewModel(m.Listing, e.Links), PriceHistory: []PricePoint{}, Matches: []FoundBy{}}
	for _, p := range m.Prices {
		d.PriceHistory = append(d.PriceHistory, PricePoint{
			PriceCents: p.PriceCents, ShippingCents: p.ShippingCents, TotalCents: p.PriceCents + Deref(p.ShippingCents),
			Currency: p.Currency, ObservedAt: p.ObservedAt.Time,
		})
	}
	for _, r := range m.Matches {
		d.Matches = append(d.Matches, FoundBy{MatchID: r.Match.ID, SearchID: r.Match.SearchID, SearchName: r.SearchName, Status: r.Match.Status})
	}
	return d
}

func (GetEndpoint) Render(d Detail) apigen.GetListingResponseObject {
	out := apigen.GetListing200JSONResponse{Listing: ToAPI(d.Listing)}
	for _, p := range d.PriceHistory {
		out.PriceHistory = append(out.PriceHistory, apigen.PricePoint{
			PriceCents: p.PriceCents, ShippingCents: p.ShippingCents, TotalCents: p.TotalCents, Currency: p.Currency, ObservedAt: p.ObservedAt,
		})
	}
	for _, m := range d.Matches {
		out.Matches = append(out.Matches, apigen.ListingMatch{MatchId: m.MatchID, SearchId: m.SearchID, SearchName: m.SearchName, Status: apigen.MatchStatus(m.Status)})
	}
	if out.PriceHistory == nil {
		out.PriceHistory = []apigen.PricePoint{}
	}
	if out.Matches == nil {
		out.Matches = []apigen.ListingMatch{}
	}
	return out
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// Deref is a nullable amount, 0 when NULL.
func Deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// TimePtr is a nullable timestamp as a *time.Time.
func TimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
