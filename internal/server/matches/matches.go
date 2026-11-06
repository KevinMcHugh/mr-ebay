// Package matches holds the endpoints for what a search has found: listing a
// search's matches, and marking them seen, starred or dismissed.
package matches

import (
	"context"
	"fmt"
	"slices"
	"time"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
)

// Statuses a match can have. Every match starts new.
var Statuses = []string{"new", "seen", "starred", "dismissed"}

// MaxIDs bounds one status change.
const MaxIDs = 500

// -----------------------------------------------------------------------------
// View model
// -----------------------------------------------------------------------------

// Match is a listing as one search found it.
type Match struct {
	ID          string           `json:"id"`
	SearchID    string           `json:"searchId"`
	Status      string           `json:"status"`
	FirstSeenAt time.Time        `json:"firstSeenAt"`
	LastSeenAt  time.Time        `json:"lastSeenAt"`
	Listing     listings.Listing `json:"listing"`
}

// ToViewModel is a stored match and its listing.
func ToViewModel(m dbgen.Match, l dbgen.Listing, links app.Links) Match {
	return Match{
		ID: m.ID, SearchID: m.SearchID, Status: m.Status,
		FirstSeenAt: m.FirstSeenAt.Time, LastSeenAt: m.LastSeenAt.Time,
		Listing: listings.ToViewModel(l, links),
	}
}

// ToAPI is m as the API type.
func ToAPI(m Match) apigen.Match {
	return apigen.Match{
		Id: m.ID, SearchId: m.SearchID, Status: apigen.MatchStatus(m.Status),
		FirstSeenAt: m.FirstSeenAt, LastSeenAt: m.LastSeenAt, Listing: listings.ToAPI(m.Listing),
	}
}

func checkStatus(s string) error {
	if !slices.Contains(Statuses, s) {
		return fmt.Errorf("%w: status %q is not one of %v", app.ErrBadInput, s, Statuses)
	}
	return nil
}

// -----------------------------------------------------------------------------
// List matches
// -----------------------------------------------------------------------------

type ListStore interface {
	GetSearch(ctx context.Context, id string) (dbgen.Search, error)
	PageMatches(ctx context.Context, searchID, status string, p app.PageRequest) (app.Page[dbgen.MatchesAfterRow], error)
}

// ListEndpoint is a page of a search's matches, newest first, optionally
// of one status.
type ListEndpoint struct {
	Store ListStore
	Links app.Links
}

// Page is the page the caller asked for.
func (ListEndpoint) Page(req apigen.ListMatchesRequestObject) app.PageRequest {
	return app.PageRequest{List: "match", Limit: req.Params.Limit, After: req.Params.After}
}

func (e ListEndpoint) Interact(ctx context.Context, req apigen.ListMatchesRequestObject) (app.Page[dbgen.MatchesAfterRow], error) {
	status := string(req.Params.Status)
	if status != "" {
		if err := checkStatus(status); err != nil {
			return app.Page[dbgen.MatchesAfterRow]{}, err
		}
	}
	if _, err := e.Store.GetSearch(ctx, req.SearchId); err != nil {
		return app.Page[dbgen.MatchesAfterRow]{}, app.NotFound(err, "search", req.SearchId)
	}
	return e.Store.PageMatches(ctx, req.SearchId, status, e.Page(req))
}

func (e ListEndpoint) Build(p app.Page[dbgen.MatchesAfterRow]) app.Page[Match] {
	return app.MapPage(p, func(r dbgen.MatchesAfterRow) Match { return ToViewModel(r.Match, r.Listing, e.Links) })
}

func (ListEndpoint) Render(p app.Page[Match]) apigen.ListMatchesResponseObject {
	return apigen.ListMatches200JSONResponse{Matches: app.MapPage(p, ToAPI).Items, Next: p.Next}
}

// -----------------------------------------------------------------------------
// Set match status
// -----------------------------------------------------------------------------

type SetStatusStore interface {
	SetMatchStatus(ctx context.Context, arg dbgen.SetMatchStatusParams) (int64, error)
}

// SetStatusEndpoint marks matches with a status. Unknown ids are skipped,
// and the count says how many changed.
type SetStatusEndpoint struct{ Store SetStatusStore }

func (e SetStatusEndpoint) Interact(ctx context.Context, req apigen.SetMatchStatusRequestObject) (int64, error) {
	b := req.Body
	if err := checkStatus(string(b.Status)); err != nil {
		return 0, err
	}
	if len(b.Ids) == 0 || len(b.Ids) > MaxIDs {
		return 0, fmt.Errorf("%w: send 1-%d ids", app.ErrBadInput, MaxIDs)
	}
	return e.Store.SetMatchStatus(ctx, dbgen.SetMatchStatusParams{Status: string(b.Status), Ids: b.Ids})
}

func (SetStatusEndpoint) Build(n int64) int64 { return n }

func (SetStatusEndpoint) Render(n int64) apigen.SetMatchStatusResponseObject {
	return apigen.SetMatchStatus200JSONResponse{Updated: n}
}
