package app

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
)

// The Page methods are the store's paged lists: each takes what the list is
// filtered by and the page the caller asked for, and returns that page. The
// keyset query (…After), its sort key and app.Paginate stay here, so a list
// endpoint depends only on "this page of this list" through its narrow
// Store interface, and a fake store can return a page outright.

// Pager is every paged list the store holds. server.Store includes it,
// and a list endpoint's own Store interface names the one it reads.
type Pager interface {
	PageSearches(ctx context.Context, p PageRequest) (Page[dbgen.SearchesAfterRow], error)
	PageMatches(ctx context.Context, searchID, status string, p PageRequest) (Page[dbgen.MatchesAfterRow], error)
	PageRuns(ctx context.Context, searchID string, p PageRequest) (Page[dbgen.SearchRun], error)
}

var _ Pager = (*Store)(nil)

// byTime is where a page of a list sorted by time starts, as the cursor
// holds it.
type byTime struct {
	At time.Time
	ID string
}

func ts(t time.Time) pgtype.Timestamptz { return pgtype.Timestamptz{Time: t, Valid: true} }

// orNil is s, or nil for "" (a filter that matches everything).
func orNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// PageSearches is a page of the saved searches, newest first, with how
// many of each one's matches are still new.
func (s *Store) PageSearches(ctx context.Context, p PageRequest) (Page[dbgen.SearchesAfterRow], error) {
	return Paginate(p,
		func(after *byTime, limit int32) ([]dbgen.SearchesAfterRow, error) {
			arg := dbgen.SearchesAfterParams{Lim: limit}
			if after != nil {
				arg.AfterCreatedAt, arg.AfterID = ts(after.At), &after.ID
			}
			return s.SearchesAfter(ctx, arg)
		},
		func(r dbgen.SearchesAfterRow) byTime { return byTime{r.CreatedAt.Time, r.ID} })
}

// PageMatches is a page of a search's matches with their listings, newest
// first, of one status ("" for every status). Matches first seen in the
// same run, which share a first_seen_at, are ordered by id.
func (s *Store) PageMatches(ctx context.Context, searchID, status string, p PageRequest) (Page[dbgen.MatchesAfterRow], error) {
	return Paginate(p,
		func(after *byTime, limit int32) ([]dbgen.MatchesAfterRow, error) {
			arg := dbgen.MatchesAfterParams{SearchID: searchID, Status: orNil(status), Lim: limit}
			if after != nil {
				arg.AfterFirstSeenAt, arg.AfterID = ts(after.At), &after.ID
			}
			return s.MatchesAfter(ctx, arg)
		},
		func(r dbgen.MatchesAfterRow) byTime { return byTime{r.Match.FirstSeenAt.Time, r.Match.ID} })
}

// PageRuns is a page of runs, newest first, for one search ("" for every
// live search).
func (s *Store) PageRuns(ctx context.Context, searchID string, p PageRequest) (Page[dbgen.SearchRun], error) {
	return Paginate(p,
		func(after *byTime, limit int32) ([]dbgen.SearchRun, error) {
			arg := dbgen.RunsAfterParams{SearchID: orNil(searchID), Lim: limit}
			if after != nil {
				arg.AfterStartedAt, arg.AfterID = ts(after.At), &after.ID
			}
			return s.RunsAfter(ctx, arg)
		},
		func(r dbgen.SearchRun) byTime { return byTime{r.StartedAt.Time, r.ID} })
}
