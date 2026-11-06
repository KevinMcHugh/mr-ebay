// Package runs holds running a saved search against eBay, and the run log
// that says whether runs are working.
package runs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/rs/xid"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// Triggers: who started a run.
const (
	Manual = "manual"
	Watch  = "watch"
)

// -----------------------------------------------------------------------------
// View model
// -----------------------------------------------------------------------------

// Run is one execution of a search.
type Run struct {
	ID             string     `json:"id"`
	SearchID       string     `json:"searchId"`
	Trigger        string     `json:"trigger"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	ResultCount    int        `json:"resultCount"`
	NewCount       int        `json:"newCount"`
	PriceDropCount int        `json:"priceDropCount"`
	Error          string     `json:"error"`
}

// Result is a run and what it found. Both lists are always present, so
// "none" is never mistaken for "not reported".
type Result struct {
	Run        Run             `json:"run"`
	NewMatches []matches.Match `json:"newMatches"`
	PriceDrops []PriceDrop     `json:"priceDrops"`
}

// PriceDrop is a match whose listing's total fell since the last run that
// saw it.
type PriceDrop struct {
	Match     matches.Match `json:"match"`
	FromCents int64         `json:"fromCents"`
	ToCents   int64         `json:"toCents"`
}

// ToViewModel is a stored run, without its matches.
func ToViewModel(r dbgen.SearchRun) Run {
	return Run{
		ID: r.ID, SearchID: r.SearchID, Trigger: r.Trigger, StartedAt: r.StartedAt.Time,
		FinishedAt: listings.TimePtr(r.FinishedAt), ResultCount: int(r.ResultCount),
		NewCount: int(r.NewCount), PriceDropCount: int(r.PriceDropCount), Error: r.Error,
	}
}

// ToAPI is r as the API type.
func ToAPI(r Run) apigen.Run {
	return apigen.Run{
		Id: r.ID, SearchId: r.SearchID, Trigger: apigen.RunTrigger(r.Trigger), StartedAt: r.StartedAt,
		FinishedAt: r.FinishedAt, ResultCount: r.ResultCount, NewCount: r.NewCount,
		PriceDropCount: r.PriceDropCount, Error: r.Error,
	}
}

// -----------------------------------------------------------------------------
// Run search
// -----------------------------------------------------------------------------

type RunStore interface {
	GetSearch(ctx context.Context, id string) (dbgen.Search, error)
	CreateRun(ctx context.Context, arg dbgen.CreateRunParams) (dbgen.SearchRun, error)
	FinishRun(ctx context.Context, arg dbgen.FinishRunParams) (dbgen.SearchRun, error)
	MarkSearchRun(ctx context.Context, id string) error
	RecordResults(ctx context.Context, searchID string, items []ebay.Item) ([]app.Recorded, error)
}

// Searcher is the eBay client.
type Searcher interface {
	Search(ctx context.Context, q ebay.Query) (ebay.Result, error)
}

// RunEndpoint runs a saved search against eBay and stores what it finds.
// The run is logged whether or not it succeeds, so a failing watcher shows
// up in list_runs with its error.
type RunEndpoint struct {
	Store RunStore
	Ebay  Searcher
	Links app.Links
	// Trigger is Manual (the default) or Watch.
	Trigger string
}

type runModel struct {
	Run      dbgen.SearchRun
	Recorded []app.Recorded
}

func (e RunEndpoint) Interact(ctx context.Context, req apigen.RunSearchRequestObject) (runModel, error) {
	search, err := e.Store.GetSearch(ctx, req.SearchId)
	if err != nil {
		return runModel{}, app.NotFound(err, "search", req.SearchId)
	}
	trigger := e.Trigger
	if trigger == "" {
		trigger = Manual
	}
	run, err := e.Store.CreateRun(ctx, dbgen.CreateRunParams{ID: xid.New().String(), SearchID: search.ID, Trigger: trigger})
	if err != nil {
		return runModel{}, err
	}
	// The log must be written even if the caller has gone, so the closing
	// writes outlive a cancelled request.
	finish := func(arg dbgen.FinishRunParams) (dbgen.SearchRun, error) {
		bg := context.WithoutCancel(ctx)
		arg.ID = run.ID
		done, err := e.Store.FinishRun(bg, arg)
		if err != nil {
			return done, err
		}
		return done, e.Store.MarkSearchRun(bg, search.ID)
	}

	res, err := e.Ebay.Search(ctx, searches.QueryOf(search))
	if err != nil {
		_, _ = finish(dbgen.FinishRunParams{Error: err.Error()})
		return runModel{}, app.Upstream(err)
	}
	recs, err := e.Store.RecordResults(ctx, search.ID, res.Items)
	if err != nil {
		_, _ = finish(dbgen.FinishRunParams{ResultCount: int32(len(res.Items)), Error: "storing results: " + err.Error()})
		return runModel{}, fmt.Errorf("storing results: %w", err)
	}
	newCount, drops := 0, 0
	for _, r := range recs {
		if r.NewMatch {
			newCount++
		}
		if dropped(r) {
			drops++
		}
	}
	run, err = finish(dbgen.FinishRunParams{ResultCount: int32(len(recs)), NewCount: int32(newCount), PriceDropCount: int32(drops)})
	return runModel{Run: run, Recorded: recs}, err
}

// dropped says a listing's total fell since a run last saw it.
func dropped(r app.Recorded) bool {
	return r.PrevTotalCents != nil && r.Listing.PriceCents+listings.Deref(r.Listing.ShippingCents) < *r.PrevTotalCents
}

func (e RunEndpoint) Build(m runModel) Result {
	vm := Result{Run: ToViewModel(m.Run), NewMatches: []matches.Match{}, PriceDrops: []PriceDrop{}}
	for _, r := range m.Recorded {
		match := matches.ToViewModel(r.Match, r.Listing, e.Links)
		if r.NewMatch {
			vm.NewMatches = append(vm.NewMatches, match)
		}
		if dropped(r) {
			vm.PriceDrops = append(vm.PriceDrops, PriceDrop{Match: match, FromCents: *r.PrevTotalCents, ToCents: match.Listing.TotalCents})
		}
	}
	return vm
}

func (RunEndpoint) Render(vm Result) apigen.RunSearchResponseObject {
	out := apigen.RunSearch200JSONResponse{Run: ToAPI(vm.Run), NewMatches: []apigen.Match{}, PriceDrops: []apigen.PriceDrop{}}
	for _, m := range vm.NewMatches {
		out.NewMatches = append(out.NewMatches, matches.ToAPI(m))
	}
	for _, d := range vm.PriceDrops {
		out.PriceDrops = append(out.PriceDrops, apigen.PriceDrop{Match: matches.ToAPI(d.Match), FromCents: d.FromCents, ToCents: d.ToCents})
	}
	return out
}

// -----------------------------------------------------------------------------
// List runs
// -----------------------------------------------------------------------------

type ListStore interface {
	PageRuns(ctx context.Context, arg dbgen.PageRunsParams) ([]dbgen.SearchRun, error)
}

// ListEndpoint is a page of recent runs, newest first, for one search or
// all.
type ListEndpoint struct{ Store ListStore }

// Page is the page the caller asked for.
func (ListEndpoint) Page(req apigen.ListRunsRequestObject) app.PageRequest {
	return app.PageRequest{List: "run", Limit: req.Params.Limit, After: req.Params.After}
}

// newest is where a page of a list sorted newest first starts.
type newest struct {
	At time.Time
	ID string
}

func (e ListEndpoint) Interact(ctx context.Context, req apigen.ListRunsRequestObject) (app.Page[dbgen.SearchRun], error) {
	var search *string
	if id := req.Params.SearchId; id != "" {
		search = &id
	}
	return app.Paginate(e.Page(req),
		func(after *newest, limit int32) ([]dbgen.SearchRun, error) {
			arg := dbgen.PageRunsParams{SearchID: search, Lim: limit}
			if after != nil {
				arg.AfterStartedAt, arg.AfterID = pgtype.Timestamptz{Time: after.At, Valid: true}, &after.ID
			}
			return e.Store.PageRuns(ctx, arg)
		},
		func(r dbgen.SearchRun) newest { return newest{r.StartedAt.Time, r.ID} })
}

func (ListEndpoint) Build(p app.Page[dbgen.SearchRun]) app.Page[Run] {
	return app.MapPage(p, ToViewModel)
}

func (ListEndpoint) Render(p app.Page[Run]) apigen.ListRunsResponseObject {
	return apigen.ListRuns200JSONResponse{Runs: app.MapPage(p, ToAPI).Items, Next: p.Next}
}
