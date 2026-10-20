// Package searches holds the endpoints for saved searches: list, get,
// create, update and delete. Running a search against eBay is a separate
// endpoint (docs/design.md).
package searches

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/xid"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/auth"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
)

// Limits, in characters.
const (
	MaxName  = 100
	MaxQuery = 300
	MaxWants = 2000
)

// The values eBay's Browse API takes for its conditions and buyingOptions
// filters.
var (
	Conditions    = []string{"NEW", "USED", "UNSPECIFIED"}
	BuyingOptions = []string{"FIXED_PRICE", "AUCTION", "BEST_OFFER"}
)

var (
	currencyRE    = regexp.MustCompile(`^[A-Z]{3}$`)
	marketplaceRE = regexp.MustCompile(`^EBAY_[A-Z]{2,3}$`)
	categoryRE    = regexp.MustCompile(`^[0-9]{1,10}$`)
)

// -----------------------------------------------------------------------------
// View model
// -----------------------------------------------------------------------------

// Search is a saved search as every surface shows it. Prices are cents of
// Currency, and 0 means no limit.
type Search struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Query           string     `json:"query"`
	CategoryIDs     []string   `json:"categoryIds"`
	Conditions      []string   `json:"conditions"`
	BuyingOptions   []string   `json:"buyingOptions"`
	MinPriceCents   int64      `json:"minPriceCents,omitempty"`
	MaxPriceCents   int64      `json:"maxPriceCents,omitempty"`
	Currency        string     `json:"currency"`
	Marketplace     string     `json:"marketplace"`
	Wants           string     `json:"wants"`
	Watch           bool       `json:"watch"`
	AlertBelowCents int64      `json:"alertBelowCents,omitempty"`
	LastRunAt       *time.Time `json:"lastRunAt,omitempty"`
	NewCount        int        `json:"newCount"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	URL             string     `json:"url"`
}

// Model is a stored search and how many of its matches are still new.
type Model struct {
	Row      dbgen.Search
	NewCount int
}

// ToViewModel is the view of a stored search.
func ToViewModel(m Model, links app.Links) Search {
	r := m.Row
	vm := Search{
		ID:              r.ID,
		Name:            r.Name,
		Query:           r.Query,
		CategoryIDs:     r.CategoryIds,
		Conditions:      r.Conditions,
		BuyingOptions:   r.BuyingOptions,
		MinPriceCents:   deref(r.MinPriceCents),
		MaxPriceCents:   deref(r.MaxPriceCents),
		Currency:        r.Currency,
		Marketplace:     r.Marketplace,
		Wants:           r.Wants,
		Watch:           r.Watch,
		AlertBelowCents: deref(r.AlertBelowCents),
		NewCount:        m.NewCount,
		CreatedAt:       r.CreatedAt.Time,
		UpdatedAt:       r.UpdatedAt.Time,
		URL:             links.Search(r.ID),
	}
	if r.LastRunAt.Valid {
		t := r.LastRunAt.Time
		vm.LastRunAt = &t
	}
	return vm
}

func toAPI(s Search) apigen.Search {
	return apigen.Search{
		Id:              s.ID,
		Name:            s.Name,
		Query:           s.Query,
		CategoryIds:     s.CategoryIDs,
		Conditions:      s.Conditions,
		BuyingOptions:   s.BuyingOptions,
		MinPriceCents:   s.MinPriceCents,
		MaxPriceCents:   s.MaxPriceCents,
		Currency:        s.Currency,
		Marketplace:     s.Marketplace,
		Wants:           s.Wants,
		Watch:           s.Watch,
		AlertBelowCents: s.AlertBelowCents,
		LastRunAt:       s.LastRunAt,
		NewCount:        s.NewCount,
		CreatedAt:       s.CreatedAt,
		UpdatedAt:       s.UpdatedAt,
		Url:             s.URL,
	}
}

// -----------------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------------

// Fields is everything about a search a person can set. Create fills it
// from the request; update fills it from the stored row and lays the
// request over it, so both go through the same clean.
type Fields struct {
	Name, Query, Currency, Marketplace, Wants     string
	CategoryIDs, Conditions, BuyingOptions        []string
	MinPriceCents, MaxPriceCents, AlertBelowCents int64
	Watch                                         bool
}

func fieldsOf(r dbgen.Search) Fields {
	return Fields{
		Name: r.Name, Query: r.Query, Currency: r.Currency, Marketplace: r.Marketplace, Wants: r.Wants,
		CategoryIDs: r.CategoryIds, Conditions: r.Conditions, BuyingOptions: r.BuyingOptions,
		MinPriceCents: deref(r.MinPriceCents), MaxPriceCents: deref(r.MaxPriceCents), AlertBelowCents: deref(r.AlertBelowCents),
		Watch: r.Watch,
	}
}

// Clean trims and normalizes f and checks every bound, returning
// app.ErrBadInput naming the first field that is wrong.
func Clean(f Fields) (Fields, error) {
	bad := func(format string, a ...any) (Fields, error) {
		return Fields{}, fmt.Errorf("%w: "+format, append([]any{app.ErrBadInput}, a...)...)
	}
	f.Name = strings.Join(strings.Fields(f.Name), " ")
	f.Query = strings.Join(strings.Fields(f.Query), " ")
	f.Wants = strings.TrimSpace(f.Wants)
	f.Currency = strings.ToUpper(strings.TrimSpace(f.Currency))
	f.Marketplace = strings.ToUpper(strings.TrimSpace(f.Marketplace))
	if f.Currency == "" {
		f.Currency = "USD"
	}
	if f.Marketplace == "" {
		f.Marketplace = "EBAY_US"
	}

	switch {
	case f.Name == "":
		return bad("name is required")
	case utf8.RuneCountInString(f.Name) > MaxName:
		return bad("name is longer than %d characters", MaxName)
	case f.Query == "":
		return bad("query is required")
	case utf8.RuneCountInString(f.Query) > MaxQuery:
		return bad("query is longer than %d characters", MaxQuery)
	case utf8.RuneCountInString(f.Wants) > MaxWants:
		return bad("wants is longer than %d characters", MaxWants)
	case !currencyRE.MatchString(f.Currency):
		return bad("currency %q is not a three-letter code like USD", f.Currency)
	case !marketplaceRE.MatchString(f.Marketplace):
		return bad("marketplace %q is not an eBay marketplace id like EBAY_US", f.Marketplace)
	case f.MinPriceCents < 0 || f.MaxPriceCents < 0 || f.AlertBelowCents < 0:
		return bad("prices cannot be negative")
	case f.MaxPriceCents != 0 && f.MinPriceCents > f.MaxPriceCents:
		return bad("minPriceCents is above maxPriceCents")
	}
	var err error
	if f.CategoryIDs, err = cleanSet("category id", f.CategoryIDs, nil, categoryRE); err != nil {
		return Fields{}, err
	}
	if f.Conditions, err = cleanSet("condition", f.Conditions, Conditions, nil); err != nil {
		return Fields{}, err
	}
	if f.BuyingOptions, err = cleanSet("buying option", f.BuyingOptions, BuyingOptions, nil); err != nil {
		return Fields{}, err
	}
	return f, nil
}

// cleanSet trims, upper-cases, sorts and de-duplicates vs, refusing any
// value outside allowed (when given) or not matching re (when given). It
// never returns nil, because the columns are NOT NULL arrays.
func cleanSet(what string, vs, allowed []string, re *regexp.Regexp) ([]string, error) {
	out := []string{}
	for _, v := range vs {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "" {
			continue
		}
		if (allowed != nil && !slices.Contains(allowed, v)) || (re != nil && !re.MatchString(v)) {
			msg := fmt.Sprintf("%s %q is not allowed", what, v)
			if allowed != nil {
				msg += "; use one of " + strings.Join(allowed, ", ")
			}
			return nil, fmt.Errorf("%w: %s", app.ErrBadInput, msg)
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// -----------------------------------------------------------------------------
// List searches
// -----------------------------------------------------------------------------

type ListStore interface {
	ListSearches(ctx context.Context) ([]dbgen.ListSearchesRow, error)
}

// ListEndpoint is every saved search, newest first.
type ListEndpoint struct {
	Store ListStore
	Links app.Links
}

func (e ListEndpoint) Interact(ctx context.Context, _ apigen.ListSearchesRequestObject) ([]Model, error) {
	rows, err := e.Store.ListSearches(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Model, len(rows))
	for i, r := range rows {
		out[i] = Model{Row: dbgen.Search{
			ID: r.ID, Name: r.Name, Query: r.Query, CategoryIds: r.CategoryIds, Conditions: r.Conditions,
			BuyingOptions: r.BuyingOptions, MinPriceCents: r.MinPriceCents, MaxPriceCents: r.MaxPriceCents,
			Currency: r.Currency, Marketplace: r.Marketplace, Wants: r.Wants, Watch: r.Watch,
			AlertBelowCents: r.AlertBelowCents, LastRunAt: r.LastRunAt, CreatedBy: r.CreatedBy,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		}, NewCount: int(r.NewCount)}
	}
	return out, nil
}

func (e ListEndpoint) Build(ms []Model) []Search {
	out := make([]Search, len(ms))
	for i, m := range ms {
		out[i] = ToViewModel(m, e.Links)
	}
	return out
}

func (e ListEndpoint) Render(vms []Search) apigen.ListSearchesResponseObject {
	out := make([]apigen.Search, len(vms))
	for i, s := range vms {
		out[i] = toAPI(s)
	}
	return apigen.ListSearches200JSONResponse{Searches: out}
}

// -----------------------------------------------------------------------------
// Get search
// -----------------------------------------------------------------------------

type GetStore interface {
	GetSearch(ctx context.Context, id string) (dbgen.Search, error)
	CountNewMatches(ctx context.Context, searchID string) (int32, error)
}

// GetEndpoint is one saved search.
type GetEndpoint struct {
	Store GetStore
	Links app.Links
}

func (e GetEndpoint) Interact(ctx context.Context, req apigen.GetSearchRequestObject) (Model, error) {
	row, err := e.Store.GetSearch(ctx, req.SearchId)
	if err != nil {
		return Model{}, app.NotFound(err, "search", req.SearchId)
	}
	n, err := e.Store.CountNewMatches(ctx, row.ID)
	return Model{Row: row, NewCount: int(n)}, err
}

func (e GetEndpoint) Build(m Model) Search { return ToViewModel(m, e.Links) }

func (e GetEndpoint) Render(vm Search) apigen.GetSearchResponseObject {
	return apigen.GetSearch200JSONResponse(toAPI(vm))
}

// -----------------------------------------------------------------------------
// Create search
// -----------------------------------------------------------------------------

type CreateStore interface {
	CreateSearch(ctx context.Context, arg dbgen.CreateSearchParams) (dbgen.Search, error)
}

// CreateEndpoint saves a search. It does not run it: that is RunEndpoint's
// job, so saving stays free and instant.
type CreateEndpoint struct {
	Store CreateStore
	Links app.Links
}

func (e CreateEndpoint) Interact(ctx context.Context, req apigen.CreateSearchRequestObject) (Model, error) {
	b := req.Body
	f, err := Clean(Fields{
		Name: b.Name, Query: b.Query, Currency: b.Currency, Marketplace: b.Marketplace, Wants: b.Wants,
		CategoryIDs: b.CategoryIds, Conditions: b.Conditions, BuyingOptions: b.BuyingOptions,
		MinPriceCents: b.MinPriceCents, MaxPriceCents: b.MaxPriceCents, AlertBelowCents: b.AlertBelowCents,
		Watch: b.Watch,
	})
	if err != nil {
		return Model{}, err
	}
	row, err := e.Store.CreateSearch(ctx, dbgen.CreateSearchParams{
		ID: xid.New().String(), Name: f.Name, Query: f.Query,
		CategoryIds: f.CategoryIDs, Conditions: f.Conditions, BuyingOptions: f.BuyingOptions,
		MinPriceCents: limit(f.MinPriceCents), MaxPriceCents: limit(f.MaxPriceCents),
		Currency: f.Currency, Marketplace: f.Marketplace, Wants: f.Wants,
		Watch: f.Watch, AlertBelowCents: limit(f.AlertBelowCents),
		CreatedBy: auth.KeyIDPtr(ctx),
	})
	return Model{Row: row}, err
}

func (e CreateEndpoint) Build(m Model) Search { return ToViewModel(m, e.Links) }

func (e CreateEndpoint) Render(vm Search) apigen.CreateSearchResponseObject {
	return apigen.CreateSearch201JSONResponse(toAPI(vm))
}

// -----------------------------------------------------------------------------
// Update search
// -----------------------------------------------------------------------------

type UpdateStore interface {
	UpdateSearch(ctx context.Context, arg dbgen.UpdateSearchParams) (dbgen.Search, error)
	GetStore
}

// UpdateEndpoint changes whichever fields the request sends. A price of 0
// removes that limit.
type UpdateEndpoint struct {
	Store UpdateStore
	Links app.Links
}

func (e UpdateEndpoint) Interact(ctx context.Context, req apigen.UpdateSearchRequestObject) (Model, error) {
	cur, err := e.Store.GetSearch(ctx, req.SearchId)
	if err != nil {
		return Model{}, app.NotFound(err, "search", req.SearchId)
	}
	f, b := fieldsOf(cur), req.Body
	set(&f.Name, b.Name)
	set(&f.Query, b.Query)
	set(&f.Currency, b.Currency)
	set(&f.Marketplace, b.Marketplace)
	set(&f.Wants, b.Wants)
	set(&f.CategoryIDs, b.CategoryIds)
	set(&f.Conditions, b.Conditions)
	set(&f.BuyingOptions, b.BuyingOptions)
	set(&f.MinPriceCents, b.MinPriceCents)
	set(&f.MaxPriceCents, b.MaxPriceCents)
	set(&f.AlertBelowCents, b.AlertBelowCents)
	set(&f.Watch, b.Watch)
	if f, err = Clean(f); err != nil {
		return Model{}, err
	}
	row, err := e.Store.UpdateSearch(ctx, dbgen.UpdateSearchParams{
		ID: cur.ID, Name: f.Name, Query: f.Query,
		CategoryIds: f.CategoryIDs, Conditions: f.Conditions, BuyingOptions: f.BuyingOptions,
		MinPriceCents: limit(f.MinPriceCents), MaxPriceCents: limit(f.MaxPriceCents),
		Currency: f.Currency, Marketplace: f.Marketplace, Wants: f.Wants,
		Watch: f.Watch, AlertBelowCents: limit(f.AlertBelowCents),
	})
	if err != nil {
		return Model{}, app.NotFound(err, "search", req.SearchId)
	}
	n, err := e.Store.CountNewMatches(ctx, row.ID)
	return Model{Row: row, NewCount: int(n)}, err
}

func (e UpdateEndpoint) Build(m Model) Search { return ToViewModel(m, e.Links) }

func (e UpdateEndpoint) Render(vm Search) apigen.UpdateSearchResponseObject {
	return apigen.UpdateSearch200JSONResponse(toAPI(vm))
}

// -----------------------------------------------------------------------------
// Delete search
// -----------------------------------------------------------------------------

type DeleteStore interface {
	DeleteSearch(ctx context.Context, id string) (int64, error)
}

// DeleteEndpoint soft-deletes a search. Its listings stay: another search
// may have found them too.
type DeleteEndpoint struct{ Store DeleteStore }

func (e DeleteEndpoint) Interact(ctx context.Context, req apigen.DeleteSearchRequestObject) (bool, error) {
	n, err := e.Store.DeleteSearch(ctx, req.SearchId)
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, fmt.Errorf("%w: search %s", app.ErrNotFound, req.SearchId)
	}
	return true, nil
}

func (DeleteEndpoint) Build(ok bool) bool { return ok }

func (DeleteEndpoint) Render(bool) apigen.DeleteSearchResponseObject {
	return apigen.DeleteSearch204Response{}
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// set overwrites *dst with *src when the request sent it.
func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// limit is a price limit as its column stores it: NULL for none.
func limit(cents int64) *int64 {
	if cents == 0 {
		return nil
	}
	return &cents
}
